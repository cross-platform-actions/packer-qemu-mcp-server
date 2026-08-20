// Package supervisor runs one detached build and holds its terminal.
//
// A build under emulation takes hours, far longer than the processes that
// watch it, so the supervisor is deliberately small: it answers the pauses
// nobody wants to see, keeps the guest frozen while an agent looks at the ones
// that matter, publishes where the build got to, and takes instructions over a
// socket. It knows nothing about MCP, and an MCP server can come and go
// without the build noticing.
package supervisor

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/cross-platform-actions/packer-qemu-mcp-server/internal/buildstate"
	"github.com/cross-platform-actions/packer-qemu-mcp-server/internal/control"
	"github.com/cross-platform-actions/packer-qemu-mcp-server/internal/guest"
	"github.com/cross-platform-actions/packer-qemu-mcp-server/internal/pause"
)

// DefaultShutdownGrace is how long Packer gets to shut the virtual machine
// down and clean up after an abort before it is killed outright. Packer needs
// the time: it has a virtual machine to stop and an output directory to
// remove.
const DefaultShutdownGrace = 30 * time.Second

// drainGrace bounds the wait for the build's last words after it has exited.
const drainGrace = 2 * time.Second

// callPatience bounds how long a control socket request waits to be dealt
// with. It is shorter than the caller's own timeout so that a request is
// dropped while the caller is still listening, rather than acted on after it
// has given up.
const callPatience = control.DefaultTimeout / 2

// maximumSettle is as long as a build may ask to be left running after a pause
// before its guest is frozen. One goroutine owns the build's state and answers
// for it, so it is not answering while it settles; a settle anywhere near
// callPatience would have a build refuse to be stopped. The freezing it exists
// to postpone happens in milliseconds, so a second is already generous.
const maximumSettle = time.Second

// An Option configures a supervisor before it starts watching.
type Option func(*Supervisor)

// ShutdownGrace sets how long the build gets to shut itself down before it is
// killed outright. A test has neither a virtual machine to stop nor an output
// directory to remove, and should not wait as though it did — and under the
// race detector a stand-in build does not even receive the interrupt, since
// the instrumented parent leaves SIGINT blocked in the child it forks, so
// every abort would otherwise cost the full grace.
func ShutdownGrace(grace time.Duration) Option {
	return func(s *Supervisor) { s.shutdownGrace = grace }
}

// Run supervises the build described by the given directory until it exits.
func Run(dir *buildstate.Dir, options ...Option) error {
	meta := dir.Meta()
	if len(meta.Command) == 0 {
		return fmt.Errorf("build %s has no command to run", dir.ID())
	}

	log, err := os.OpenFile(dir.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("opening the build log: %w", err)
	}
	defer log.Close()

	terminal, err := Open(command(dir, meta))
	if err != nil {
		return err
	}

	supervisor := &Supervisor{
		dir:           dir,
		terminal:      terminal,
		guest:         guest.At(dir.MonitorSocket()),
		log:           log,
		freezing:      meta.Freezing(),
		settle:        min(meta.Settle, maximumSettle),
		shutdownGrace: DefaultShutdownGrace,
		pauses:        make(chan pause.Pause, 64),
		calls:         make(chan call),
		exited:        make(chan int, 1),
		outputDone:    make(chan struct{}),
		stopped:       make(chan struct{}),
	}
	for _, option := range options {
		option(supervisor)
	}
	return supervisor.run()
}

// command is the build itself, with the environment the harness depends on.
func command(dir *buildstate.Dir, meta buildstate.Meta) *exec.Cmd {
	built := exec.Command(meta.Command[0], meta.Command[1:]...)
	built.Dir = meta.WorkingDir
	built.Env = append(os.Environ(),
		// Nothing human reads this terminal, and escape codes only make the
		// log harder to read back.
		"PACKER_NO_COLOR=1",
		// Packer echoes its own prompts into its log. Sending that log to a
		// file rather than to the terminal keeps every pause prompt on the
		// terminal exactly once, so that no pause is ever answered twice.
		"PACKER_LOG=1",
		"PACKER_LOG_PATH="+dir.PackerLogPath(),
	)
	for name, value := range meta.Env {
		built.Env = append(built.Env, name+"="+value)
	}
	return built
}

// Supervisor is one build and everything watching it.
type Supervisor struct {
	dir           *buildstate.Dir
	terminal      *Terminal
	guest         *guest.Guest
	log           io.Writer
	freezing      bool
	settle        time.Duration
	shutdownGrace time.Duration

	scanner pause.Scanner
	state   buildstate.State

	pauses     chan pause.Pause
	calls      chan call
	exited     chan int
	outputDone chan struct{}
	stopped    chan struct{}
}

// call is a control socket request waiting for the one goroutine that owns the
// build's state to answer it.
type call struct {
	method   string
	params   json.RawMessage
	deadline time.Time
	reply    chan answered
}

// expired reports whether the caller has given up. Acting on a call that
// nobody is listening for any more is worse than dropping it: an agent told
// that its abort failed, whose build then stops anyway, has no way to tell
// what state anything is in.
func (c call) expired() bool {
	return time.Now().After(c.deadline)
}

type answered struct {
	result any
	err    error
}

func (s *Supervisor) run() error {
	defer s.terminal.Close()

	s.state = buildstate.State{SupervisorPID: os.Getpid(), BuildPID: s.terminal.PID()}
	s.publish()

	// Read from the start: a terminal nobody reads fills up and stops the
	// build mid-sentence, including while it is being shut down below.
	go s.read()

	server, err := control.Listen(s.dir.ControlSocket(), s.enqueue)
	if err != nil {
		// The build is already running, and without a socket nothing could
		// ever reach it again. Take it down rather than leave it.
		s.note(err)
		s.finish(s.stop())
		return err
	}
	defer server.Close()

	go func() { s.exited <- s.terminal.Wait() }()

	for {
		select {
		case paused := <-s.pauses:
			s.reached(paused)
		case request := <-s.calls:
			if request.expired() {
				continue
			}
			result, err := s.dispatch(request.method, request.params)
			request.reply <- answered{result: result, err: err}
		case status := <-s.exited:
			s.drain()
			s.finish(status)
			close(s.stopped)
			return nil
		}
	}
}

// read copies the build's terminal to its log, watching it go past for pause
// prompts.
func (s *Supervisor) read() {
	defer close(s.outputDone)

	buffer := make([]byte, 4096)
	for {
		read, err := s.terminal.Read(buffer)
		if read > 0 {
			// Losing a line of the log is regrettable; failing to answer the
			// pause it announced would strand the build.
			_, _ = s.log.Write(buffer[:read])
			for _, found := range s.scanner.Scan(buffer[:read]) {
				s.pauses <- found
			}
		}
		if err != nil {
			return
		}
	}
}

// drain gives the build's last output time to reach the log. Any pause in it
// is past answering: the build is already gone.
func (s *Supervisor) drain() {
	deadline := time.After(drainGrace)
	for {
		select {
		case <-s.outputDone:
			return
		case <-s.pauses:
		case <-deadline:
			return
		}
	}
}

// reached handles a pause: the ones nobody wants to step through are answered
// on the spot, and the rest freeze the guest and wait for an agent.
func (s *Supervisor) reached(found pause.Pause) {
	s.state.Step = found.Step
	s.state.Label = found.Label()
	s.state.Note = ""

	if !found.Interesting() {
		s.pass()
		return
	}
	if s.runningPast(found) {
		s.photograph(found)
		s.pass()
		return
	}

	s.state.Paused = true
	s.hold()
	s.publish()
}

// photograph records a boot step the build is being run past. The step is not
// stopped at and the guest is not frozen — the build is on its way somewhere —
// but the screen at the end of it is kept.
//
// Running forward is how a boot command of sixty steps is worked with at all,
// and without this it is also how the evidence is lost: a desync begins at one
// step and is noticed at another, and a picture only of where the build ended
// up says nothing about which step went wrong. A failure to capture leaves a
// frame with no picture rather than no frame, since a gap in the trail is
// itself worth seeing.
func (s *Supervisor) photograph(found pause.Pause) {
	frame := buildstate.Frame{Label: found.Label()}

	path := s.dir.NewScreenshotPath()
	if err := s.guest.Capture(path); err == nil {
		frame.Screenshot = path
	}
	s.state.Trail = append(s.state.Trail, frame)
}

// pass answers a pause without stopping at it.
func (s *Supervisor) pass() {
	s.state.Paused = false
	if err := s.terminal.Answer(); err != nil {
		// The build is still where it was, so what was photographed there is
		// still what is on the screen.
		s.note(err)
		s.publish()
		return
	}
	s.forgetTheScreenshot()
	s.publish()
}

// forgetTheScreenshot drops the capture from the pause the build is leaving.
// It was taken at a step that is now over, and offering it as the current
// screen would have an agent reasoning about one moment while looking at
// another — the very confusion this is here to clear up.
func (s *Supervisor) forgetTheScreenshot() {
	s.state.Screenshot = ""
	s.state.ScreenshotLabel = ""
}

// runningPast reports whether this pause is one to answer on the way to the
// step a caller named.
//
// A named step that never arrives would otherwise run the build past every
// boot step and into StepConnect, where nothing pauses and the guest is left
// unwatched, so the end of the boot command stops it regardless.
func (s *Supervisor) runningPast(found pause.Pause) bool {
	if s.state.Awaiting == "" {
		return false
	}
	if found.Matches(s.state.Awaiting) {
		s.state.Awaiting = ""
		return false
	}
	if found.EndsBootCommand() {
		s.note(fmt.Errorf("the boot command ended without reaching %q", s.state.Awaiting))
		s.state.Awaiting = ""
		return false
	}
	return true
}

// hold freezes the guest and photographs it. Neither is possible before Packer
// has started the virtual machine, and a pause without a screenshot is still
// worth more than no pause at all, so failure is reported rather than raised.
//
// A build may ask for a moment's grace before the freeze. Packer announces a
// boot step the instant it has finished writing its keystrokes, which is not
// the instant the guest has finished reading them, and stopping the virtual
// CPUs in between is suspected of losing the tail of a step. The grace is off
// unless asked for, because waiting also lets the screen move on from the one
// those keystrokes were typed into.
func (s *Supervisor) hold() {
	if s.settle > 0 {
		time.Sleep(s.settle)
	}
	if s.freezing {
		if err := s.guest.Freeze(); err != nil {
			s.note(fmt.Errorf("could not freeze the guest: %w", err))
		}
	}
	s.state.Frozen = s.guest.Frozen()
	s.forgetTheScreenshot()

	path := s.dir.NewScreenshotPath()
	if err := s.guest.Capture(path); err != nil {
		s.note(fmt.Errorf("could not photograph the guest: %w", err))
		return
	}
	s.state.Screenshot = path
	s.state.ScreenshotLabel = s.state.Label
}

func (s *Supervisor) dispatch(method string, params json.RawMessage) (any, error) {
	switch method {
	case "continue":
		return nil, s.resume(instructions(params).UntilLabel)
	case "abort":
		return nil, s.abort()
	case "status":
		return s.state, nil
	default:
		return nil, fmt.Errorf("unknown method %q", method)
	}
}

// resume starts the guest before the build, so that the keystrokes Packer is
// about to type reach a guest that can consume them.
func (s *Supervisor) resume(until string) error {
	if !s.state.Paused {
		return fmt.Errorf("the build is not paused: it is %s. A step that ends in a wait runs "+
			"for as long as that wait, and there is nothing to let go of until it reaches "+
			"its pause", s.doing())
	}

	s.state.Note = ""
	s.state.Awaiting = until
	// The trail describes the leg the build is about to travel, not the last
	// one.
	s.state.Trail = nil
	if err := s.guest.Resume(); err != nil {
		s.note(fmt.Errorf("could not resume the guest: %w", err))
	}
	s.state.Frozen = s.guest.Frozen()

	if err := s.terminal.Answer(); err != nil {
		// The build is still sitting at its prompt, so a guest left running
		// now would run unattended: a boot menu would count down, and the
		// keystrokes still to come would land somewhere else entirely. Put it
		// back the way it was.
		s.refreeze()
		s.publish()
		return err
	}
	s.state.Paused = false
	s.forgetTheScreenshot()
	s.publish()
	return nil
}

// doing is what the build is up to, for telling a caller who asked for
// something the build is in no position to be asked. A step still running is
// the ordinary case, and reads as a fault unless it is named as what it is.
func (s *Supervisor) doing() string {
	switch {
	case s.state.Finished:
		return fmt.Sprintf("finished, with exit status %d", s.state.ExitCode)
	case s.state.Awaiting != "":
		return fmt.Sprintf("running forward to %q, last past %q",
			s.state.Awaiting, s.state.Label)
	case s.state.Label != "":
		return fmt.Sprintf("running, last past %q", s.state.Label)
	default:
		return "running, and has not reached a pause yet"
	}
}

// refreeze restores the invariant after a resume that could not be finished.
func (s *Supervisor) refreeze() {
	if err := s.guest.Freeze(); err != nil {
		s.note(fmt.Errorf("could not freeze the guest again: %w", err))
	}
	s.state.Frozen = s.guest.Frozen()
}

// stop ends the build and waits for it, for the case where the supervisor
// itself cannot carry on.
func (s *Supervisor) stop() int {
	_ = s.terminal.Terminate(s.shutdownGrace)
	return s.terminal.Wait()
}

// abort stops the build, never leaving a guest frozen behind it.
func (s *Supervisor) abort() error {
	s.state.Awaiting = ""
	if err := s.guest.Resume(); err != nil {
		s.note(fmt.Errorf("could not resume the guest: %w", err))
	}
	s.state.Frozen = s.guest.Frozen()
	s.state.Paused = false
	s.publish()

	return s.terminal.Terminate(s.shutdownGrace)
}

func (s *Supervisor) finish(status int) {
	s.state.Paused = false
	s.state.Awaiting = ""
	s.state.Frozen = false
	// There is no guest left to be a picture of.
	s.forgetTheScreenshot()
	s.state.Finished = true
	s.state.ExitCode = status
	s.state.FinishedAt = time.Now().UTC()
	s.publish()
}

// enqueue hands a control socket request to the goroutine that owns the
// build's state, and gives up before the caller does.
func (s *Supervisor) enqueue(method string, params json.RawMessage) (any, error) {
	request := call{
		method:   method,
		params:   params,
		deadline: time.Now().Add(callPatience),
		reply:    make(chan answered, 1),
	}

	select {
	case s.calls <- request:
	case <-s.stopped:
		return nil, fmt.Errorf("the build has finished")
	case <-time.After(callPatience):
		return nil, fmt.Errorf("the supervisor is busy with the guest and did not get to %s", method)
	}

	select {
	case answer := <-request.reply:
		return answer.result, answer.err
	case <-time.After(callPatience):
		return nil, fmt.Errorf("%s is taking too long", method)
	}
}

// Instructions are the parameters a call may carry.
type Instructions struct {
	// UntilLabel names the boot step to run forward to, answering every pause
	// before it rather than stopping.
	UntilLabel string `json:"until_label,omitempty"`
}

func instructions(params json.RawMessage) Instructions {
	var carried Instructions
	if len(params) > 0 {
		_ = json.Unmarshal(params, &carried)
	}
	return carried
}

func (s *Supervisor) note(err error) {
	if err == nil {
		return
	}
	s.state.Note = strings.TrimPrefix(s.state.Note+"; "+err.Error(), "; ")
}

func (s *Supervisor) publish() {
	if err := s.dir.PublishState(s.state); err != nil {
		fmt.Fprintf(os.Stderr, "publishing build state: %s\n", err)
	}
}
