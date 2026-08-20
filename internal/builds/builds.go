// Package builds starts, finds and drives detached Packer builds.
//
// Everything here is deliberately stateless. A build outlives the process that
// started it — that is the whole point of detaching it — so builds are
// rediscovered from disk rather than remembered, and instructions reach them
// over their supervisor's socket.
package builds

import (
	"context"
	"errors"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cross-platform-actions/packer-qemu-mcp-server/internal/buildstate"
	"github.com/cross-platform-actions/packer-qemu-mcp-server/internal/control"
	"github.com/cross-platform-actions/packer-qemu-mcp-server/internal/guest"
	"github.com/cross-platform-actions/packer-qemu-mcp-server/internal/keyboard"
	"github.com/cross-platform-actions/packer-qemu-mcp-server/internal/qmp"
	"github.com/cross-platform-actions/packer-qemu-mcp-server/internal/supervisor"
)

// defaultMonitorVar is the Packer variable that carries the harness's own
// monitor socket path into a template.
const defaultMonitorVar = "qmp_socket"

// defaultStartupWait is how long a supervisor gets to come up and say so.
const defaultStartupWait = 10 * time.Second

// defaultSettle is how long a fresh build is watched for an immediate death,
// so that a template Packer refuses to run is reported to whoever asked for
// the build instead of only to the log.
const defaultSettle = 1500 * time.Millisecond

// observeTimeout bounds asking a supervisor what it is doing. A supervisor
// that is not busy answers immediately, and listing every build on the host
// must not wait on the ones that are.
const observeTimeout = 2 * time.Second

// defaultStopWait is how long an abort waits for the build to be gone. Packer
// has a virtual machine to shut down and an output directory to remove, and a
// caller told that the build has stopped while that directory is still being
// deleted will start its next build into the wreckage.
const defaultStopWait = supervisor.DefaultShutdownGrace + 15*time.Second

// pauseTick is how often a wait looks to see whether the build has arrived at
// a pause. It reads a file rather than asking the supervisor, so that waiting
// costs the build nothing.
const pauseTick = 200 * time.Millisecond

// staleAfter is how long a finished build goes on being listed. A host
// accumulates builds, and last week's has nothing to do with the question
// being asked today.
const staleAfter = 24 * time.Hour

// packerLogTail is how much of Packer's own log a failed start carries back.
const packerLogTail = 30

// keyInterval is the least gap left between one key press and the next, so
// that a guest emulated on a slow host has time to take them in. A press held
// for longer than that sets the gap instead, or the next press would be sent
// while this one was still down.
const keyInterval = 50 * time.Millisecond

// Launcher starts a build's supervisor.
type Launcher interface {
	Launch(dir *buildstate.Dir) error
}

// Service is the whole tool surface, minus the protocol that carries it.
type Service struct {
	root     *buildstate.Root
	packer   string
	launcher Launcher
	startup  time.Duration
	settle   time.Duration
	stopWait time.Duration
}

// New returns a service that keeps its builds under the given root and runs
// them with the given packer binary.
func New(root *buildstate.Root, packer string) *Service {
	return &Service{
		root:     root,
		packer:   packer,
		launcher: &DetachedLauncher{},
		startup:  defaultStartupWait,
		settle:   defaultSettle,
		stopWait: defaultStopWait,
	}
}

// Request is a build to start.
type Request struct {
	Template   string
	Vars       map[string]string
	WorkingDir string
	Env        map[string]string
	ExtraArgs  []string
	// VarFiles are Packer variable files, passed as -var-file. A template of
	// any size keeps its variables in one.
	VarFiles []string
	// Debug runs the build under `-debug`, which is what makes stepping
	// through the boot command possible. It defaults to on.
	Debug *bool
	// MonitorVar names the template variable that receives the monitor socket
	// path. An empty name passes no variable, for templates that open the
	// socket some other way.
	MonitorVar *string
	// Freeze stops the guest while the build waits at a pause. It defaults to
	// on, and is worth turning off only to find out whether the freezing
	// itself is disturbing a boot command.
	Freeze *bool
	// Settle is how long the guest is left running after a pause before it is
	// frozen.
	Settle time.Duration
}

// Build is a build that has been started.
type Build struct {
	Meta buildstate.Meta
	Dir  *buildstate.Dir
}

// Listing is what a look at the host's builds turned up.
type Listing struct {
	Builds []Summary `json:"builds"`
	// Omitted counts the builds left out for being from another day, so that a
	// short list is not mistaken for an empty host.
	Omitted int `json:"omitted,omitempty"`
}

// Summary is one line about a build.
type Summary struct {
	ID        string    `json:"id"`
	Template  string    `json:"template"`
	StartedAt time.Time `json:"started_at"`
	Elapsed   string    `json:"elapsed"`
	// Step and Label are the last pause the build reached, which is behind
	// where it is now: a step is announced as it finishes.
	Step   string `json:"last_pause_step,omitempty"`
	Label  string `json:"last_pause,omitempty"`
	Paused bool   `json:"paused"`
	// Awaiting is the step the build is running forward to, if any.
	Awaiting   string    `json:"awaiting,omitempty"`
	Frozen     bool      `json:"frozen"`
	Finished   bool      `json:"finished"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	ExitCode   int       `json:"exit_code,omitempty"`
}

// Report is everything worth knowing about one build.
type Report struct {
	Summary
	Directory string `json:"directory"`
	// Guest is what the emulator says about the virtual machine: running,
	// paused, or gone. A build waiting for SSH cannot tell those apart.
	Guest      string `json:"guest"`
	Supervised bool   `json:"supervised"`
	// Screenshot is the capture taken at the pause the build is waiting at,
	// and ScreenshotLabel is the step it belongs to. Neither survives the
	// build moving on.
	Screenshot      string `json:"screenshot,omitempty"`
	ScreenshotLabel string `json:"screenshot_label,omitempty"`
	// PackerLog is the file Packer logs to, which is the only place QEMU's own
	// messages appear. A virtual machine that refuses to start explains itself
	// there and nowhere else.
	PackerLog string `json:"packer_log"`
	// Trail is the boot steps the build was run past on its way here, each
	// with the screen as that step finished.
	Trail []buildstate.Frame `json:"trail,omitempty"`
	Note  string             `json:"note,omitempty"`
	Log   []string           `json:"log,omitempty"`
}

// Start launches a build, detached, and waits only long enough to know that it
// is on its feet.
func (s *Service) Start(request Request) (*Build, error) {
	template, err := filepath.Abs(request.Template)
	if err != nil {
		return nil, fmt.Errorf("resolving the template path: %w", err)
	}
	if _, err := os.Stat(template); err != nil {
		return nil, fmt.Errorf("cannot read the template %s: %w", template, err)
	}

	dir, err := s.root.Create(buildstate.NewID(template))
	if err != nil {
		return nil, err
	}

	meta := buildstate.Meta{
		ID:         dir.ID(),
		Template:   template,
		WorkingDir: workingDir(request, template),
		Command:    s.command(request, dir, template),
		Env:        request.Env,
		StartedAt:  time.Now().UTC(),
		Freeze:     request.Freeze,
		Settle:     request.Settle,
		Monitored:  monitorVar(request) != "",
	}
	if err := dir.WriteMeta(meta); err != nil {
		return nil, err
	}
	if err := s.launcher.Launch(dir); err != nil {
		return nil, err
	}
	if err := s.awaitStartup(dir); err != nil {
		return nil, err
	}
	return &Build{Meta: meta, Dir: dir}, nil
}

// command is the packer invocation, with the template last.
func (s *Service) command(request Request, dir *buildstate.Dir, template string) []string {
	command := []string{s.packer, "build"}
	if request.Debug == nil || *request.Debug {
		command = append(command, "-debug")
	}
	if name := monitorVar(request); name != "" {
		command = append(command, "-var", name+"="+dir.MonitorSocket())
	}
	for _, file := range request.VarFiles {
		command = append(command, "-var-file", file)
	}
	for _, name := range sortedNames(request.Vars) {
		command = append(command, "-var", name+"="+request.Vars[name])
	}
	command = append(command, request.ExtraArgs...)
	return append(command, template)
}

// awaitStartup waits until the build can be reached, and then keeps watching
// just long enough to catch a build that dies on the spot — a template Packer
// refuses, most often — so that whoever asked for the build hears about it
// rather than only the log.
func (s *Service) awaitStartup(dir *buildstate.Dir) error {
	if !await(s.startup, func() bool { return listening(dir) || dir.State().Finished }) {
		return fmt.Errorf("the supervisor for build %s never started; see %s", dir.ID(), dir.SupervisorLogPath())
	}

	if await(s.settle, func() bool { return dir.State().Finished }) {
		if state := dir.State(); state.ExitCode != 0 {
			return fmt.Errorf("the build stopped immediately with exit status %d:\n%s\n\n"+
				"Packer's log, which is the only place QEMU's own messages appear, is %s:\n%s",
				state.ExitCode, strings.Join(dir.TailLog(20), "\n"),
				dir.PackerLogPath(), strings.Join(dir.TailPackerLog(packerLogTail), "\n"))
		}
	}
	return nil
}

// List reports the builds the root knows about, most recent first. Builds that
// finished long enough ago to belong to another day's work are left out unless
// asked for: a host keeps every build it has ever run, and a listing of all of
// them buries the one being worked on.
func (s *Service) List(all bool) (*Listing, error) {
	dirs, err := s.root.List()
	if err != nil {
		return nil, err
	}

	listing := &Listing{Builds: make([]Summary, 0, len(dirs))}
	for _, dir := range dirs {
		state, _ := observe(dir)
		summary := summarise(dir, state)
		if all || !summary.stale() {
			listing.Builds = append(listing.Builds, summary)
			continue
		}
		listing.Omitted++
	}
	return listing, nil
}

// stale reports whether a build is old news: finished, and finished long
// enough ago that nobody asking about builds now means this one.
//
// When it finished, not when it started. A build takes hours under emulation,
// and one that ran overnight and stopped a minute ago is the build being
// worked on.
func (s Summary) stale() bool {
	if !s.Finished {
		return false
	}
	if s.FinishedAt.IsZero() {
		return time.Since(s.StartedAt) > staleAfter
	}
	return time.Since(s.FinishedAt) > staleAfter
}

// Status reports where a build got to, including the last few lines of its
// terminal and what the emulator makes of the guest.
func (s *Service) Status(id string, logLines int) (*Report, error) {
	dir, err := s.root.Open(id)
	if err != nil {
		return nil, err
	}

	state, supervised := observe(dir)
	return &Report{
		Summary:         summarise(dir, state),
		Directory:       dir.Path(),
		Guest:           condition(dir),
		Supervised:      supervised,
		Screenshot:      state.Screenshot,
		ScreenshotLabel: state.ScreenshotLabel,
		PackerLog:       dir.PackerLogPath(),
		Trail:           state.Trail,
		Note:            note(state, supervised),
		Log:             dir.TailLog(logLines),
	}, nil
}

// WaitForPause waits until the build stops at a pause, or finishes, or the
// wait runs out, and reports where it got to either way.
//
// Polling status through a whole ten-minute installation is a great many turns
// spent learning nothing. Running out of time is not an error: the answer is
// the report, which says whether the build is paused.
func (s *Service) WaitForPause(ctx context.Context, id string, within time.Duration, logLines int) (*Report, error) {
	dir, err := s.root.Open(id)
	if err != nil {
		return nil, err
	}

	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		state := dir.State()
		if state.Paused || state.Finished {
			break
		}
		// A supervisor that has died leaves the state file where it was, and
		// nothing will ever change it. Waiting out the hour to learn that
		// helps nobody; the report says the build is unattended.
		if !listening(dir) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pauseTick):
		}
	}
	return s.Status(id, logLines)
}

// SendKeys types at the guest through the emulated keyboard, which needs
// nothing running inside the guest and works at a boot prompt or an installer
// menu, where nothing else can be asked anything.
func (s *Service) SendKeys(id, text string, keys []string, hold time.Duration) error {
	dir, err := s.root.Open(id)
	if err != nil {
		return err
	}

	presses, err := keyboard.Type(text)
	if err != nil {
		return err
	}
	named, err := keyboard.Keys(keys)
	if err != nil {
		return err
	}
	presses = append(presses, named...)
	if len(presses) == 0 {
		return fmt.Errorf("nothing to type: give text, keys, or both")
	}

	watched := guest.At(dir.MonitorSocket())
	if err := awake(dir, watched); err != nil {
		return err
	}
	if err := watched.Type(presses, hold, keyInterval); err != nil {
		return err
	}

	// Typing a line takes seconds, and the build is running throughout: it can
	// reach a pause part way through and have its guest frozen underneath the
	// rest, which reads as keystrokes that went astray.
	if err := awake(dir, watched); err != nil {
		return fmt.Errorf("all %d key presses were sent, but the guest stopped reading "+
			"part way through: %w", len(presses), err)
	}
	return nil
}

// Clicked is where a click landed, and the screen it landed on.
type Clicked struct {
	X int `json:"x"`
	Y int `json:"y"`
	// Screen is the guest's screen as it was just before the click, which is
	// both how the point was measured and a record of what was clicked on.
	Screen guest.Screen `json:"screen"`
}

// Click presses a mouse button at a point on the guest's screen, given in
// pixels of the screenshot it was read off. The screen is photographed first,
// both to learn how big it is — the emulated tablet reports a position as a
// fraction of the screen and knows nothing of pixels — and so that there is a
// record of what was clicked on.
func (s *Service) Click(id string, at image.Point, button string, times int) (*Clicked, error) {
	dir, err := s.root.Open(id)
	if err != nil {
		return nil, err
	}

	watched := guest.At(dir.MonitorSocket())
	if err := awake(dir, watched); err != nil {
		return nil, err
	}

	screen, err := watched.Photograph(dir.NewScreenshotPath())
	if err != nil {
		return nil, monitorError(dir, err, "photograph")
	}
	if !screen.Holds(at) {
		return nil, fmt.Errorf("(%d, %d) is not on a screen that is %dx%d",
			at.X, at.Y, screen.Width, screen.Height)
	}
	if err := watched.Point(screen, at, button, times); err != nil {
		return nil, refusedClick(err)
	}
	return &Clicked{X: at.X, Y: at.Y, Screen: screen}, nil
}

// refusedClick adds what a refusal almost always means. Only a monitor that
// answered gets the explanation: a socket that was busy or was not there says
// nothing about what devices the template has, and sending an agent to add a
// tablet it already has is worse than saying nothing.
func refusedClick(err error) error {
	if errors.Is(err, qmp.ErrBusy) || errors.Is(err, qmp.ErrUnavailable) {
		return err
	}
	return fmt.Errorf("%w; a click needs an absolute pointing device, which the default "+
		"PS/2 mouse is not: add -device qemu-xhci and -device usb-tablet to qemuargs", err)
}

// awake refuses to send input to a guest whose virtual CPUs are stopped. The
// keystrokes would sit in the emulated keyboard until the guest ran again, and
// nothing on the screen would move, which reads as input that went astray.
func awake(dir *buildstate.Dir, watched *guest.Guest) error {
	condition, err := watched.Condition()
	if err != nil {
		return monitorError(dir, err, "send anything to")
	}
	if !condition.Running {
		return fmt.Errorf("the guest is %s, so it would not read anything sent to it; "+
			"continue the build first, or start it with freeze off", condition.Name)
	}
	return nil
}

// Screenshot photographs the guest's screen and returns the file it wrote.
//
// It works whenever QEMU is running, which is the mode that matters: under
// emulation the installation itself happens inside a single Packer step, where
// `-debug` offers no pause at all. It never changes whether the guest is
// running.
func (s *Service) Screenshot(id string) (string, error) {
	dir, err := s.root.Open(id)
	if err != nil {
		return "", err
	}

	path := dir.NewScreenshotPath()
	if err := guest.At(dir.MonitorSocket()).Capture(path); err != nil {
		return "", monitorError(dir, err, "photograph")
	}
	return path, nil
}

// Continue resumes a frozen guest and lets the paused build carry on.
//
// A named step runs the build forward to it, answering every pause on the way.
// An installer's boot command can be sixty steps long, and stopping at each of
// them to reach the one that matters is not stepping through a build so much
// as retyping it.
func (s *Service) Continue(id, untilLabel string) error {
	return s.tell(id, "continue", supervisor.Instructions{UntilLabel: untilLabel})
}

// Abort resumes a frozen guest, stops the build, and waits for it to be gone
// before saying so.
//
// The wait is the point. Packer shuts the virtual machine down and deletes its
// output directory on the way out, and a build started into that directory
// while the previous one is still deleting it fails to launch, for a reason
// that looks nothing like the cause. Running out of patience is reported
// rather than raised: the report says whether the build has finished.
func (s *Service) Abort(ctx context.Context, id string, logLines int) (*Report, error) {
	dir, err := s.root.Open(id)
	if err != nil {
		return nil, err
	}
	if err := s.tell(id, "abort", nil); err != nil {
		return nil, err
	}

	deadline := time.Now().Add(s.stopWait)
	for time.Now().Before(deadline) && !dir.State().Finished {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(tick):
		}
	}
	return s.Status(id, logLines)
}

func (s *Service) tell(id, method string, params any) error {
	dir, err := s.root.Open(id)
	if err != nil {
		return err
	}

	err = control.Dial(dir.ControlSocket()).Call(method, params, nil)
	if errors.Is(err, control.ErrNoSupervisor) {
		return unreachable(dir, method)
	}
	return err
}

// observe prefers the supervisor's own account of the build, falling back to
// what it last published, and says whether the supervisor is still there.
//
// Only a supervisor that cannot be reached at all counts as gone. One that is
// merely slow to answer — it is single-threaded, and freezing and
// photographing a wedged guest can take seconds — is still minding the build,
// and saying otherwise would send an agent looking for a problem that is not
// there.
func observe(dir *buildstate.Dir) (buildstate.State, bool) {
	client := control.Dial(dir.ControlSocket())
	client.Timeout = observeTimeout

	var live buildstate.State
	switch err := client.Call("status", nil, &live); {
	case err == nil:
		return live, true
	case errors.Is(err, control.ErrNoSupervisor):
		return dir.State(), false
	default:
		return dir.State(), true
	}
}

// guest is what the emulator says about the virtual machine. A monitor that
// cannot be reached is not one answer but three, and they mean different
// things: no virtual machine yet, no virtual machine any more, and a monitor
// somebody else is holding. Reporting all three as unreachable sends an agent
// looking for a fault during the seconds before QEMU has started listening.
func condition(dir *buildstate.Dir) string {
	status, err := guest.At(dir.MonitorSocket()).Condition()
	switch {
	case err == nil:
		return status.Name
	case errors.Is(err, qmp.ErrBusy):
		return "busy"
	case !errors.Is(err, qmp.ErrUnavailable):
		return "unreachable"
	case dir.State().Finished:
		return "gone"
	case !dir.Meta().Monitored:
		return "no monitor socket"
	default:
		return "not started"
	}
}

// unreachable explains a build that cannot be told anything. Usually it has
// simply finished; if it has not, its supervisor died and left it running with
// nobody to answer its pauses.
func unreachable(dir *buildstate.Dir, method string) error {
	state := dir.State()
	if state.Finished {
		return fmt.Errorf("build %s has finished; nothing is left to %s", dir.ID(), method)
	}
	return fmt.Errorf("build %s cannot be reached: its supervisor is gone, "+
		"and the build itself (pid %d) may still be running", dir.ID(), state.BuildPID)
}

// note warns about a build nothing is watching any more, on top of whatever
// the supervisor had to say for itself.
func note(state buildstate.State, supervised bool) string {
	if supervised || state.Finished {
		return state.Note
	}
	warning := fmt.Sprintf("the supervisor is gone; the build (pid %d) is unattended "+
		"and will not stop at any further pause", state.BuildPID)
	if state.Note == "" {
		return warning
	}
	return state.Note + "; " + warning
}

// monitorError explains a monitor that could not be reached in terms of the
// build, and in terms of what was being asked of it: an agent told that a
// build it tried to type at has no guest left to photograph is being told
// about the wrong thing.
func monitorError(dir *buildstate.Dir, err error, attempted string) error {
	if !errors.Is(err, qmp.ErrUnavailable) {
		return err
	}
	if state := dir.State(); state.Finished {
		return fmt.Errorf("%w: build %s has finished, so there is no guest left to %s",
			err, dir.ID(), attempted)
	}
	if !dir.Meta().Monitored {
		return fmt.Errorf("%w: build %s was started without a monitor socket, so its guest "+
			"cannot be reached at all", err, dir.ID())
	}
	return fmt.Errorf("%w: the build has not started the virtual machine yet", err)
}

// summarise takes the id from the directory rather than from the metadata:
// a build whose meta.json is missing or half-written — a directory created a
// moment ago, or left behind by a server that died writing it — must still be
// listed under a name the other tools accept.
func summarise(dir *buildstate.Dir, state buildstate.State) Summary {
	meta := dir.Meta()
	return Summary{
		ID:         dir.ID(),
		Template:   meta.Template,
		StartedAt:  meta.StartedAt,
		Elapsed:    elapsed(meta, state),
		Step:       state.Step,
		Label:      state.Label,
		Paused:     state.Paused,
		Awaiting:   state.Awaiting,
		Frozen:     state.Frozen,
		Finished:   state.Finished,
		FinishedAt: state.FinishedAt,
		ExitCode:   state.ExitCode,
	}
}

func elapsed(meta buildstate.Meta, state buildstate.State) string {
	if meta.StartedAt.IsZero() {
		return ""
	}
	end := time.Now().UTC()
	if state.Finished && !state.FinishedAt.IsZero() {
		end = state.FinishedAt
	}
	return end.Sub(meta.StartedAt).Round(time.Second).String()
}

// listening reports whether the build's supervisor is there to be told
// anything.
func listening(dir *buildstate.Dir) bool {
	_, err := os.Stat(dir.ControlSocket())
	return err == nil
}

// tick is how often a wait looks again at something that changes on disk.
const tick = 20 * time.Millisecond

func await(within time.Duration, reached func() bool) bool {
	deadline := time.Now().Add(within)
	for {
		if reached() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(tick)
	}
}

func workingDir(request Request, template string) string {
	if request.WorkingDir != "" {
		return request.WorkingDir
	}
	return filepath.Dir(template)
}

func monitorVar(request Request) string {
	if request.MonitorVar == nil {
		return defaultMonitorVar
	}
	return *request.MonitorVar
}

// sortedNames keeps a build's command line stable whatever order the variables
// arrived in.
func sortedNames(vars map[string]string) []string {
	names := make([]string, 0, len(vars))
	for name := range vars {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
