package supervisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/cross-platform-actions/packer-qemu-mcp-server/internal/buildstate"
	"github.com/cross-platform-actions/packer-qemu-mcp-server/internal/control"
	"github.com/cross-platform-actions/packer-qemu-mcp-server/internal/packertest"
	"github.com/cross-platform-actions/packer-qemu-mcp-server/internal/qmptest"
)

// patience is how long to wait for something a build does, generous enough for
// the emulated guests CI runs these in. See the note in the builds package.
const (
	patience = 30 * time.Second
	tick     = 20 * time.Millisecond
)

// shutdownGrace is what the supervisors here are given: nothing in these tests
// has a virtual machine to shut down.
const shutdownGrace = 250 * time.Millisecond

// Every script below stands in for `packer build -debug`: it prints the pause
// prompts Packer prints, and blocks on the terminal until answered. Packer
// only pauses on a real terminal, which is the whole reason a supervisor
// exists, so these tests exercise the terminal for real.
var (
	ordinaryPause = packertest.Pause(
		`==> qemu.probe: Pausing after run of step 'StepDownload'. Press enter to continue. `)
	bootStepPause = packertest.BootStepPause("boot menu")
	cleanupPause  = packertest.Pause(
		`\n==> qemu.probe: Pausing before cleanup of step 'stepRun'. Press enter to continue. `)
)

func TestOrdinaryPausesAreAnsweredWithoutAsking(t *testing.T) {
	// Downloading an ISO and creating a disk pause too, and nobody wants to
	// step through those.
	build := start(t, script(ordinaryPause, `printf "\n==> qemu.probe: carried on\n"`))

	require.NoError(t, build.wait())

	require.Contains(t, build.log(), "carried on")
	require.Equal(t, 0, build.state().ExitCode)
	require.True(t, build.state().Finished)
}

func TestCleanupPausesAreAnsweredWithoutAsking(t *testing.T) {
	// Freezing a guest that is being torn down would hang the build.
	build := start(t, script(cleanupPause, `printf "\n==> qemu.probe: cleaned up\n"`))

	require.NoError(t, build.wait())

	require.Contains(t, build.log(), "cleaned up")
	require.False(t, build.state().Frozen)
}

func TestABuildNothingCouldReachIsNotLeftRunning(t *testing.T) {
	build := newBuild(t, script(`sleep 60`))
	// Something else is already listening where the supervisor would, so the
	// supervisor cannot come up — and a build nobody can ever reach again is
	// worse than no build.
	blocker, err := control.Listen(build.dir.ControlSocket(), func(string, json.RawMessage) (any, error) {
		return nil, nil
	})
	require.NoError(t, err)
	defer blocker.Close()

	require.Error(t, Run(build.dir, ShutdownGrace(shutdownGrace)))

	state := build.state()
	require.NotZero(t, state.BuildPID)
	require.False(t, running(state.BuildPID))
	require.True(t, state.Finished)
}

func TestBootStepPauseFreezesAndPhotographsTheGuest(t *testing.T) {
	build := start(t, script(bootStepPause, `printf "\n==> qemu.probe: carried on\n"`))

	build.awaitPause()

	state := build.state()
	require.True(t, state.Frozen)
	require.Equal(t, "boot menu", state.Label)
	require.Contains(t, build.monitor.Executed(), "stop")
	require.Contains(t, build.monitor.Executed(), "screendump")
	require.Equal(t, state.Screenshot, build.monitor.Arguments("screendump")["filename"])
	require.NotContains(t, build.log(), "carried on", "the build ran on through the pause")
}

func TestContinueResumesTheGuestAndThenTheBuild(t *testing.T) {
	build := start(t, script(bootStepPause, `printf "\n==> qemu.probe: carried on\n"`))
	build.awaitPause()

	require.NoError(t, build.call("continue"))

	require.NoError(t, build.wait())
	require.Contains(t, build.log(), "carried on")
	require.False(t, build.state().Frozen)
	require.False(t, build.state().Paused)
	// The guest must be running again before Packer is: a frozen guest cannot
	// answer the keystrokes Packer is about to type at it.
	require.Equal(t, []string{"stop", "cont"}, runStateChanges(build.monitor.Executed()))
}

func TestAPauseThatCannotBePhotographedOffersNoOlderPicture(t *testing.T) {
	build := start(t, script(bootStepPause, `printf "\n"`, bootStepPause, `printf "\n"`))
	build.awaitPause()
	require.NotEmpty(t, build.state().Screenshot)

	// The screen of one boot step is not evidence about another, and an agent
	// told to read it would be diagnosing the wrong picture.
	build.monitor.Fail("screendump", "GenericError", "no vga device")
	require.NoError(t, build.call("continue"))
	build.awaitPauseWithNothingToShow()

	require.Empty(t, build.state().Screenshot)
	build.abort()
}

// A build that is moving has no screen worth showing: whatever was captured
// belongs to a step that is over, and an agent reading it as the current one
// is looking at the wrong moment. That was reported from real use.
func TestLettingTheBuildGoDropsThePictureOfWhereItWas(t *testing.T) {
	build := start(t, script(bootStep("boot menu"), endOfBootCommand()))
	build.awaitPause()
	first := build.state()
	require.NotEmpty(t, first.Screenshot)
	require.Equal(t, first.Label, first.ScreenshotLabel)

	require.NoError(t, build.callUntil("continue", "nothing by this name"))

	// The end of the boot command stops a build running forward to a step that
	// never arrives, and it is not the step the picture was taken at.
	build.awaitPause()
	require.NotEqual(t, first.Screenshot, build.state().Screenshot)
	build.abort()
}

// A desync begins at one boot step and is noticed at another. Running forward
// is how a boot command of sixty steps is worked with at all, and it used to
// be how the fifty steps in between were lost.
func TestTheStepsRunPastArePhotographedOnTheWay(t *testing.T) {
	build := start(t, script(
		bootStep("Installation messages in English"),
		bootStep("Custom installation"),
		bootStep("Compiler tools"),
		endOfBootCommand()))
	build.awaitPause()

	require.NoError(t, build.callUntil("continue", "Compiler tools"))
	require.Eventually(t, func() bool {
		return build.state().Label == "Compiler tools" && build.state().Paused
	}, patience, tick, "the build never reached the step it was sent to")

	// The two steps in between, each with the screen as it finished, and not
	// the one it stopped at: that one has a screenshot of its own.
	trail := build.state().Trail
	require.Equal(t, []string{"Custom installation"}, labels(trail))
	require.NotEmpty(t, trail[0].Screenshot)
	require.NotEqual(t, trail[0].Screenshot, build.state().Screenshot)
	build.abort()
}

// Passing a step is not stopping at one: the guest is on its way somewhere and
// must not be held still for a photograph.
func TestTheStepsRunPastAreNotFrozenForTheirPicture(t *testing.T) {
	build := start(t, script(
		bootStep("first"),
		bootStep("second"),
		endOfBootCommand()))
	build.awaitPause()
	before := len(stopsAndStarts(build.monitor.Executed()))

	require.NoError(t, build.callUntil("continue", "nothing by this name"))
	build.awaitPause()

	// Exactly one thaw for the continue and one freeze at the end of the boot
	// command, whatever happened in between.
	require.Equal(t, before+2, len(stopsAndStarts(build.monitor.Executed())))
	require.NotEmpty(t, build.state().Trail)
	build.abort()
}

// The trail describes the leg the build is on, not every leg it has travelled.
func TestTheTrailStartsAgainAtEveryContinue(t *testing.T) {
	build := start(t, script(
		bootStep("first"),
		bootStep("second"),
		bootStep("third"),
		endOfBootCommand()))
	build.awaitPause()
	require.NoError(t, build.callUntil("continue", "third"))
	require.Eventually(t, func() bool {
		return build.state().Label == "third" && build.state().Paused
	}, patience, tick, "the build never reached the third step")
	require.NotEmpty(t, build.state().Trail)

	require.NoError(t, build.call("continue"))
	build.awaitPause()

	require.Empty(t, build.state().Trail)
	build.abort()
}

// Freezing the guest is the whole point of the harness, and it is also the
// prime suspect when a boot command comes out wrong under -debug: the tool
// that exists to watch the keystrokes would be changing them. Being able to
// run the same build without it is how one finds out which.
func TestABuildMayBeToldToLeaveItsGuestRunning(t *testing.T) {
	build := newBuild(t, script(bootStepPause, `printf "\n"`))
	build.reviseMeta(func(meta *buildstate.Meta) { meta.Freeze = new(bool) })
	build.monitor = qmptest.StartAt(t, build.dir.MonitorSocket())
	build.run()

	build.awaitPause()

	state := build.state()
	require.False(t, state.Frozen)
	require.NotContains(t, build.monitor.Executed(), "stop")
	// The picture is still taken: it is the pause that is being watched, not
	// the freezing.
	require.NotEmpty(t, state.Screenshot)
	build.abort()
}

// Packer announces a boot step the instant it has finished writing its
// keystrokes, which is not the instant the guest has finished reading them.
func TestABuildMayAskForAMomentBeforeTheFreeze(t *testing.T) {
	const settle = 500 * time.Millisecond

	build := newBuild(t, script(bootStepPause, `printf "\n"`))
	build.reviseMeta(func(meta *buildstate.Meta) { meta.Settle = settle })
	build.monitor = qmptest.StartAt(t, build.dir.MonitorSocket())
	build.run()

	require.Eventually(t, func() bool {
		return strings.Contains(build.log(), "Pausing after run of step")
	}, patience, tick, "the build never announced its pause")

	// The pause has been announced and the guest is still running: without the
	// settle it would already have been stopped, since that happens in the
	// same breath as reading the prompt.
	require.NotContains(t, build.monitor.Executed(), "stop",
		"the guest was frozen before it had a moment to take the keystrokes in")

	build.awaitPause()
	require.Contains(t, build.monitor.Executed(), "stop")
	require.True(t, build.state().Frozen)
	build.abort()
}

// A boot command with three labelled steps, of the shape an installer has.
func bootStep(label string) string {
	return packertest.BootStepPause(label) + `; printf "\n"`
}

func endOfBootCommand() string {
	return packertest.Pause(
		`==> qemu.probe: Pausing after run of step 'stepTypeBootCommand'. Press enter to continue. `) +
		`; printf "\n"`
}

func TestContinueRunsForwardToTheStepItIsGiven(t *testing.T) {
	// An installer's boot command runs to dozens of steps; stopping at each of
	// them to reach the one that matters is not stepping through a build.
	build := start(t, script(
		bootStep("Installation messages in English"),
		bootStep("Custom installation"),
		bootStep("Compiler tools"),
		endOfBootCommand()))
	build.awaitPause()
	require.Equal(t, "Installation messages in English", build.state().Label)

	require.NoError(t, build.callUntil("continue", "Compiler tools"))

	build.awaitPauseAt("Compiler tools")
	state := build.state()
	require.True(t, state.Frozen, "the step it was sent to should be frozen like any other")
	require.Empty(t, state.Awaiting, "it has arrived")
	require.NotEmpty(t, state.Screenshot)
	build.abort()
}

func TestStepsRunPastAreNotFrozenOrPhotographed(t *testing.T) {
	build := start(t, script(
		bootStep("first"),
		bootStep("second"),
		bootStep("third"),
		endOfBootCommand()))
	build.awaitPause()
	freezesBefore := len(runStateChanges(build.monitor.Executed()))

	require.NoError(t, build.callUntil("continue", "third"))
	build.awaitPauseAt("third")

	// One thaw to get going and one freeze on arrival; nothing for the step in
	// between.
	require.Equal(t, freezesBefore+2, len(runStateChanges(build.monitor.Executed())))
	build.abort()
}

func TestRunningForwardIsVisibleWhileItHappens(t *testing.T) {
	build := start(t, script(
		bootStep("first"),
		`sleep 2`,
		bootStep("second"),
		endOfBootCommand()))
	build.awaitPause()

	require.NoError(t, build.callUntil("continue", "second"))

	require.Equal(t, "second", build.state().Awaiting)
	build.awaitPauseAt("second")
	build.abort()
}

func TestAStepThatNeverArrivesStopsAtTheEndOfTheBootCommand(t *testing.T) {
	// Running on into StepConnect would leave the guest unwatched with
	// ssh_timeout ticking, which is the one thing this must not do.
	build := start(t, script(
		bootStep("first"),
		bootStep("second"),
		endOfBootCommand(),
		`printf "==> qemu.probe: Waiting for SSH\n"; sleep 30`))
	build.awaitPause()

	require.NoError(t, build.callUntil("continue", "a step that is not there"))

	build.awaitPauseAt("stepTypeBootCommand")
	state := build.state()
	require.True(t, state.Frozen)
	require.Empty(t, state.Awaiting)
	require.Contains(t, state.Note, "without reaching")
	build.abort()
}

func TestContinueIsRefusedWhenTheBuildIsNotWaiting(t *testing.T) {
	build := start(t, script(`sleep 30`))

	err := build.call("continue")

	require.ErrorContains(t, err, "not paused")
	build.abort()
}

func TestAbortResumesTheGuestAndStopsTheBuild(t *testing.T) {
	build := start(t, script(bootStepPause, `printf "\n==> qemu.probe: carried on\n"`))
	build.awaitPause()

	require.NoError(t, build.call("abort"))

	require.NoError(t, build.wait())
	// A build left frozen would keep a virtual machine pinned to a CPU
	// forever.
	require.Contains(t, build.monitor.Executed(), "cont")
	require.True(t, build.state().Finished)
	require.NotContains(t, build.log(), "carried on")
}

func TestTheLastKnownStepSurvivesTheBuild(t *testing.T) {
	build := start(t, script(ordinaryPause, `exit 3`))

	require.NoError(t, build.wait())

	state := build.state()
	require.Equal(t, "StepDownload", state.Step)
	require.Equal(t, 3, state.ExitCode)
	require.True(t, state.Finished)
	require.False(t, state.FinishedAt.IsZero())
}

func TestAFinishedBuildStopsAnsweringCalls(t *testing.T) {
	build := start(t, script(`exit 0`))
	require.NoError(t, build.wait())

	err := build.call("continue")

	require.ErrorIs(t, err, control.ErrNoSupervisor)
}

func TestABuildWithoutAMonitorStillPauses(t *testing.T) {
	// Before Packer starts the virtual machine there is no QEMU to freeze,
	// and a boot step can only be reached once there is. A monitor that never
	// appears must not cost the agent the pause itself.
	build := startWithoutMonitor(t, script(bootStepPause, `printf "\n==> qemu.probe: carried on\n"`))

	build.awaitPause()

	state := build.state()
	require.True(t, state.Paused)
	require.False(t, state.Frozen)
	require.Contains(t, state.Note, "monitor")

	require.NoError(t, build.call("continue"))
	require.NoError(t, build.wait())
	require.Contains(t, build.log(), "carried on")
}

func TestTheBuildIsReportedWhileItRuns(t *testing.T) {
	build := start(t, script(bootStepPause, `printf "\n"`))
	build.awaitPause()

	var reported buildstate.State
	require.NoError(t, build.callResult("status", &reported))

	require.True(t, reported.Paused)
	require.Equal(t, "boot menu", reported.Label)
	require.NotZero(t, reported.BuildPID)
	build.abort()
}

// supervisedBuild is a build running under a supervisor, with a fake QEMU
// monitor behind it.
type supervisedBuild struct {
	t       *testing.T
	dir     *buildstate.Dir
	monitor *qmptest.Server
	done    chan error
	started bool
	reaped  bool
}

func start(t *testing.T, body string) *supervisedBuild {
	t.Helper()

	build := newBuild(t, body)
	build.monitor = qmptest.StartAt(t, build.dir.MonitorSocket())
	return build.run()
}

func startWithoutMonitor(t *testing.T, body string) *supervisedBuild {
	t.Helper()

	return newBuild(t, body).run()
}

func newBuild(t *testing.T, body string) *supervisedBuild {
	t.Helper()

	root, err := os.MkdirTemp("/tmp", "sup")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(root) })

	dir, err := buildstate.New(root).Create("build-1")
	require.NoError(t, err)

	fake := filepath.Join(root, "fake-packer")
	require.NoError(t, os.WriteFile(fake, []byte(body), 0o700))
	require.NoError(t, dir.WriteMeta(buildstate.Meta{
		ID:         "build-1",
		Template:   "fake.pkr.hcl",
		WorkingDir: root,
		Command:    []string{"/bin/sh", fake},
		StartedAt:  time.Now().UTC(),
	}))

	build := &supervisedBuild{t: t, dir: dir, done: make(chan error, 1)}
	t.Cleanup(func() {
		// Whatever the test did or did not do, the supervisor is finished with
		// the directory before the directory goes.
		if !build.started || build.reaped {
			return
		}
		if !build.state().Finished {
			_ = build.call("abort")
		}
		select {
		case <-build.done:
		case <-time.After(shutdownGrace + patience):
		}
	})
	return build
}

// reviseMeta changes what the build was asked to do, before it is asked.
func (b *supervisedBuild) reviseMeta(revise func(*buildstate.Meta)) {
	b.t.Helper()

	meta := b.dir.Meta()
	revise(&meta)
	require.NoError(b.t, b.dir.WriteMeta(meta))
}

func (b *supervisedBuild) run() *supervisedBuild {
	b.started = true
	go func() { b.done <- Run(b.dir, ShutdownGrace(shutdownGrace)) }()
	require.Eventually(b.t, func() bool {
		_, err := os.Stat(b.dir.ControlSocket())
		return err == nil || b.state().Finished
	}, patience, tick, "the supervisor never started listening")
	return b
}

func (b *supervisedBuild) wait() error {
	b.t.Helper()

	select {
	case err := <-b.done:
		b.reaped = true
		return err
	case <-time.After(shutdownGrace + patience):
		b.t.Fatalf("the build never finished\nstate: %+v\nlog:\n%s",
			b.state(), b.log())
		return nil
	}
}

func (b *supervisedBuild) awaitPause() {
	b.t.Helper()

	require.Eventually(b.t, func() bool {
		return b.state().Paused
	}, patience, tick, "the build never paused")
}

// awaitPauseWithNothingToShow waits for a pause the guest could not be
// photographed at, which is the one that says so.
func labels(trail []buildstate.Frame) []string {
	found := make([]string, 0, len(trail))
	for _, frame := range trail {
		found = append(found, frame.Label)
	}
	return found
}

func stopsAndStarts(executed []string) []string {
	var found []string
	for _, was := range executed {
		if was == "stop" || was == "cont" {
			found = append(found, was)
		}
	}
	return found
}

func (b *supervisedBuild) awaitPauseWithNothingToShow() {
	b.t.Helper()

	require.Eventually(b.t, func() bool {
		state := b.state()
		return state.Paused && strings.Contains(state.Note, "photograph")
	}, patience, tick, "the build never reached a pause it could not be photographed at")
}

func (b *supervisedBuild) call(method string) error {
	return control.Dial(b.dir.ControlSocket()).Call(method, nil, nil)
}

func (b *supervisedBuild) callUntil(method, label string) error {
	return control.Dial(b.dir.ControlSocket()).
		Call(method, Instructions{UntilLabel: label}, nil)
}

// awaitPauseAt waits for the build to stop at the step with the given label.
func (b *supervisedBuild) awaitPauseAt(label string) {
	b.t.Helper()

	require.Eventually(b.t, func() bool {
		state := b.state()
		return state.Paused && state.Label == label
	}, patience, tick,
		"the build never stopped at %q (it is at %q)", label, b.state().Label)
}

func (b *supervisedBuild) callResult(method string, out any) error {
	return control.Dial(b.dir.ControlSocket()).Call(method, nil, out)
}

func (b *supervisedBuild) abort() {
	b.t.Helper()

	require.NoError(b.t, b.call("abort"))
	require.NoError(b.t, b.wait())
}

func (b *supervisedBuild) state() buildstate.State {
	return b.dir.State()
}

// running reports whether a process is still there to be signalled.
func running(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func (b *supervisedBuild) log() string {
	contents, err := os.ReadFile(b.dir.LogPath())
	require.NoError(b.t, err)
	return string(contents)
}

func script(lines ...string) string {
	return "#!/bin/sh\n" + strings.Join(lines, "\n") + "\n"
}

// runStateChanges keeps only the commands that change whether the guest's
// virtual CPUs are running.
func runStateChanges(executed []string) []string {
	var changes []string
	for _, command := range executed {
		if command == "stop" || command == "cont" {
			changes = append(changes, command)
		}
	}
	return changes
}
