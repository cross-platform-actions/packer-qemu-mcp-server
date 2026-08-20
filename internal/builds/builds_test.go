package builds

import (
	"context"
	"encoding/json"
	"errors"
	"image"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/cross-platform-actions/packer-qemu-mcp-server/internal/buildstate"
	"github.com/cross-platform-actions/packer-qemu-mcp-server/internal/control"
	"github.com/cross-platform-actions/packer-qemu-mcp-server/internal/packertest"
	"github.com/cross-platform-actions/packer-qemu-mcp-server/internal/qmp"
	"github.com/cross-platform-actions/packer-qemu-mcp-server/internal/qmptest"
	"github.com/cross-platform-actions/packer-qemu-mcp-server/internal/supervisor"
)

// patience is how long to wait for something a build does. CI runs these
// inside emulated guests, where a build that takes under two seconds here has
// taken over ten, so the wait is generous; it costs nothing when the condition
// is met, since that ends the wait.
const patience = 30 * time.Second

// shutdownGrace is what the supervisors here are given: nothing in these tests
// has a virtual machine to shut down.
const shutdownGrace = 250 * time.Millisecond

// drainAllowance covers the moment between a build exiting and its supervisor
// saying so, which is spent reading the last of the terminal.
const drainAllowance = 5 * time.Second

func TestStartRunsPackerInDebugModeWithItsOwnMonitor(t *testing.T) {
	service := newService(t)
	path := template(t)

	build, err := service.Start(Request{Template: path, Vars: map[string]string{"disk": "10G"}})

	require.NoError(t, err)
	command := strings.Join(build.Meta.Command, " ")
	require.Contains(t, command, "build -debug")
	// Packer holds its own monitor connection, so the harness asks for a
	// second socket of its own through the template.
	require.Contains(t, command, "-var qmp_socket="+build.Dir.MonitorSocket())
	require.Contains(t, command, "-var disk=10G")
	require.True(t, strings.HasSuffix(command, path), command)
}

func TestStartPassesVariableFilesInTheOrderGiven(t *testing.T) {
	service := newService(t)

	build, err := service.Start(Request{
		Template: template(t),
		VarFiles: []string{"var_files/common.pkrvars.hcl", "var_files/x86-64.pkrvars.hcl"},
	})

	require.NoError(t, err)
	require.Contains(t, strings.Join(build.Meta.Command, " "),
		"-var-file var_files/common.pkrvars.hcl -var-file var_files/x86-64.pkrvars.hcl")
}

func TestStartCanLeaveDebugModeOff(t *testing.T) {
	service := newService(t)
	off := false

	build, err := service.Start(Request{Template: template(t), Debug: &off})

	require.NoError(t, err)
	require.NotContains(t, strings.Join(build.Meta.Command, " "), "-debug")
}

func TestStartCanLeaveTheMonitorVariableOut(t *testing.T) {
	service := newService(t)
	none := ""

	build, err := service.Start(Request{Template: template(t), MonitorVar: &none})

	require.NoError(t, err)
	require.NotContains(t, strings.Join(build.Meta.Command, " "), "qmp_socket")
}

func TestStartRefusesATemplateThatIsNotThere(t *testing.T) {
	service := newService(t)

	_, err := service.Start(Request{Template: "/no/such/template.pkr.hcl"})

	require.ErrorContains(t, err, "template.pkr.hcl")
}

func TestStartRunsFromTheTemplateDirectoryByDefault(t *testing.T) {
	service := newService(t)
	path := template(t)

	build, err := service.Start(Request{Template: path})

	require.NoError(t, err)
	require.Equal(t, filepath.Dir(path), build.Meta.WorkingDir)
}

func TestStartReportsASupervisorThatNeverCameUp(t *testing.T) {
	service := newService(t)
	service.launcher = launcherFunc(func(*buildstate.Dir) error { return nil })
	// Nothing is coming, so there is nothing to be patient for.
	service.startup = time.Second

	_, err := service.Start(Request{Template: template(t)})

	require.ErrorContains(t, err, "supervisor")
}

func TestStatusReportsWhereAPausedBuildGotTo(t *testing.T) {
	service := newService(t)
	build := startPaused(t, service)

	report, err := service.Status(build.Meta.ID, 10)

	require.NoError(t, err)
	require.True(t, report.Paused)
	require.True(t, report.Frozen)
	require.Equal(t, "boot menu", report.Label)
	require.Equal(t, "paused", report.Guest)
	require.NotEmpty(t, report.Screenshot)
	require.Contains(t, strings.Join(report.Log, "\n"), "Pausing after run of step")
	require.NotZero(t, report.Elapsed)
}

func TestStatusOfAnUnknownBuildIsAnError(t *testing.T) {
	service := newService(t)

	_, err := service.Status("nope", 10)

	require.ErrorContains(t, err, "nope")
}

func TestStatusOfAFinishedBuildKeepsReporting(t *testing.T) {
	service := newService(t)
	build := startPaused(t, service)
	stopped, err := service.Abort(t.Context(), build.Meta.ID, 0)
	require.NoError(t, err)
	require.True(t, stopped.Finished)

	report, err := service.Status(build.Meta.ID, 10)

	require.NoError(t, err)
	require.True(t, report.Finished)
	require.False(t, report.Paused)
	require.Equal(t, "boot menu", report.Label)
}

func TestStatusWarnsAboutABuildNobodyIsWatching(t *testing.T) {
	// A supervisor that dies leaves the build running with nothing to answer
	// its pauses, which is not the same thing as a build that has finished.
	service := newService(t)
	build := startPaused(t, service)
	orphan(t, build.Dir)

	report, err := service.Status(build.Meta.ID, 10)

	require.NoError(t, err)
	require.False(t, report.Supervised)
	require.False(t, report.Finished)
	require.Contains(t, report.Note, "unattended")
}

func TestStatusDoesNotMistakeABusySupervisorForADeadOne(t *testing.T) {
	// Freezing and photographing a wedged guest can take seconds, and the
	// supervisor answers nothing while it does. Reporting that as an abandoned
	// build would send an agent chasing a problem that is not there.
	service := newService(t)
	build := startPaused(t, service)
	require.NoError(t, os.Remove(build.Dir.ControlSocket()))
	busy, err := control.Listen(build.Dir.ControlSocket(), func(string, json.RawMessage) (any, error) {
		time.Sleep(time.Minute)
		return nil, nil
	})
	require.NoError(t, err)
	defer busy.Close()

	report, err := service.Status(build.Meta.ID, 5)

	require.NoError(t, err)
	require.True(t, report.Supervised)
	require.NotContains(t, report.Note, "unattended")
}

func TestContinueOfABuildNobodyIsWatchingSaysSo(t *testing.T) {
	service := newService(t)
	build := startPaused(t, service)
	orphan(t, build.Dir)

	err := service.Continue(build.Meta.ID, "")

	require.ErrorContains(t, err, "supervisor is gone")
	require.NotContains(t, err.Error(), "finished")
}

func TestContinueCarriesTheStepToRunTo(t *testing.T) {
	service := newService(t)
	build := startPaused(t, service)

	require.NoError(t, service.Continue(build.Meta.ID, "carried on"))

	// The supervisor took the instruction, not just the bare continue.
	require.Eventually(t, func() bool {
		state := build.Dir.State()
		return state.Awaiting == "carried on" || state.Finished
	}, patience, tick)
}

func TestContinueLetsTheBuildRunOn(t *testing.T) {
	service := newService(t)
	build := startPaused(t, service)

	require.NoError(t, service.Continue(build.Meta.ID, ""))

	awaitFinished(t, build.Dir)
	require.False(t, build.Dir.State().Frozen)
}

func TestContinueOfAFinishedBuildSaysSo(t *testing.T) {
	service := newService(t)
	build := startPaused(t, service)
	_, err := service.Abort(t.Context(), build.Meta.ID, 0)
	require.NoError(t, err)

	err = service.Continue(build.Meta.ID, "")

	require.ErrorContains(t, err, "finished")
}

func TestScreenshotWorksWhileTheBuildIsRunning(t *testing.T) {
	// This is the mode that matters: under emulation the installation itself
	// happens inside one step, where -debug offers no pause at all.
	service := newService(t)
	build := startPaused(t, service)

	path, err := service.Screenshot(build.Meta.ID)

	require.NoError(t, err)
	require.Equal(t, build.Dir.ScreenshotDir(), filepath.Dir(path))
	require.Equal(t, path, build.monitor.Arguments("screendump")["filename"])
	require.Equal(t, "png", build.monitor.Arguments("screendump")["format"])
}

func TestATemplateWithNoMonitorSocketSaysSoRatherThanWaitingToStart(t *testing.T) {
	service := newService(t)
	none := ""
	build, err := service.Start(Request{
		Template:   template(t),
		MonitorVar: &none,
		Env:        map[string]string{"TRIGGER": neverPath(t)},
	})
	require.NoError(t, err)
	t.Cleanup(func() { stop(t, service, build) })

	report, err := service.Status(build.Meta.ID, 5)

	require.NoError(t, err)
	require.Equal(t, "no monitor socket", report.Guest)
}

// Being told that a build one tried to type at has no guest left to photograph
// is being told about the wrong thing.
func TestSendKeysToAFinishedBuildDoesNotTalkAboutPhotographs(t *testing.T) {
	service := newService(t)
	build, err := service.Start(Request{Template: template(t), Env: map[string]string{"TRIGGER": neverPath(t)}})
	require.NoError(t, err)
	stopped, err := service.Abort(t.Context(), build.Meta.ID, 0)
	require.NoError(t, err)
	require.True(t, stopped.Finished)

	err = service.SendKeys(build.Meta.ID, "ok", nil, 0)

	require.ErrorContains(t, err, "send anything to")
	require.NotContains(t, err.Error(), "photograph")
}

func TestScreenshotBeforeTheVirtualMachineExistsExplainsItself(t *testing.T) {
	service := newService(t)
	build, err := service.Start(Request{Template: template(t), Env: map[string]string{"TRIGGER": neverPath(t)}})
	require.NoError(t, err)
	t.Cleanup(func() { stop(t, service, build) })

	_, err = service.Screenshot(build.Meta.ID)

	require.ErrorIs(t, err, qmp.ErrUnavailable)
}

func TestListShowsTheNewestBuildFirst(t *testing.T) {
	service := newService(t)
	older, err := service.Start(Request{Template: template(t), Env: map[string]string{"TRIGGER": neverPath(t)}})
	require.NoError(t, err)
	newer := startPaused(t, service)
	t.Cleanup(func() { stop(t, service, older) })

	listed, err := service.List(false)

	require.NoError(t, err)
	require.Equal(t, []string{newer.Meta.ID, older.Meta.ID}, summaryIDs(listed.Builds))
	require.Equal(t, "boot menu", listed.Builds[0].Label)
}

// The whole point of waiting: Packer deletes its output directory on the way
// out, and a caller told that the build has stopped while that is still going
// on will start its next build into the wreckage.
func TestAbortWaitsForTheBuildToBeGone(t *testing.T) {
	service := newService(t)
	build := startPaused(t, service)

	report, err := service.Abort(t.Context(), build.Meta.ID, 0)

	require.NoError(t, err)
	require.True(t, report.Finished, "abort returned while the build was still running")
	require.False(t, report.Frozen)
}

// QEMU's messages reach Packer's log and nothing else, so a virtual machine
// that refuses to start is explained there or nowhere.
func TestStatusPointsAtPackersOwnLog(t *testing.T) {
	service := newService(t)
	build := startPaused(t, service)

	report, err := service.Status(build.Meta.ID, 10)

	require.NoError(t, err)
	require.Equal(t, build.Dir.PackerLogPath(), report.PackerLog)
}

func TestABuildThatDiesOnTheSpotCarriesPackersLog(t *testing.T) {
	service := newService(t)
	service.packer = refusingPacker(t)
	// The whole exit, reap, drain and publish has to fit inside this, and the
	// shortened one the other tests run with is not the one that ships.
	service.settle = defaultSettle

	_, err := service.Start(Request{Template: template(t)})

	require.ErrorContains(t, err, "qemu-system-x86_64: -display gtk: unsupported")
}

// A monitor that is not there yet is not a fault, and reporting it as one
// sends an agent looking for a problem during the seconds before QEMU has
// started listening.
func TestGuestIsNotStartedBeforeThereIsAVirtualMachine(t *testing.T) {
	service := newService(t)
	build, err := service.Start(Request{Template: template(t), Env: map[string]string{"TRIGGER": neverPath(t)}})
	require.NoError(t, err)
	t.Cleanup(func() { stop(t, service, build) })

	report, err := service.Status(build.Meta.ID, 5)

	require.NoError(t, err)
	require.Equal(t, "not started", report.Guest)
}

func TestListLeavesOutBuildsFromAnotherDay(t *testing.T) {
	service := newService(t)
	current := startPaused(t, service)
	ancient := yesterdaysBuild(t, service, buildstate.State{Finished: true})

	listed, err := service.List(false)
	require.NoError(t, err)
	require.Equal(t, []string{current.Meta.ID}, summaryIDs(listed.Builds))
	require.Equal(t, 1, listed.Omitted)

	everything, err := service.List(true)
	require.NoError(t, err)
	require.Contains(t, summaryIDs(everything.Builds), ancient)
	require.Zero(t, everything.Omitted)
}

func TestListKeepsALongBuildThatHasJustFinished(t *testing.T) {
	service := newService(t)
	overnight := yesterdaysBuild(t, service, buildstate.State{
		Finished: true, FinishedAt: time.Now().UTC().Add(-time.Minute),
	})

	listed, err := service.List(false)

	require.NoError(t, err)
	require.Contains(t, summaryIDs(listed.Builds), overnight)
}

func TestWaitForPauseReturnsWhenTheBuildArrives(t *testing.T) {
	service := newService(t)
	service.packer = twicePausingPacker(t)
	build := startPaused(t, service)
	require.NoError(t, service.Continue(build.Meta.ID, ""))

	report, err := service.WaitForPause(t.Context(), build.Meta.ID, patience, 5)

	require.NoError(t, err)
	require.True(t, report.Paused, "the build never reached its second pause")
	require.Equal(t, "second thoughts", report.Label)
}

// Nothing will ever move a state file whose supervisor has died, and waiting
// out the hour to discover that helps nobody.
func TestWaitForPauseGivesUpOnABuildNobodyIsWatching(t *testing.T) {
	service := newService(t)
	build := startPaused(t, service)
	require.NoError(t, service.Continue(build.Meta.ID, "never arrives"))
	orphan(t, build.Dir)

	report, err := service.WaitForPause(t.Context(), build.Meta.ID, patience, 5)

	require.NoError(t, err)
	require.False(t, report.Supervised)
	require.Contains(t, report.Note, "unattended")
}

// Running out of time is not an error: the report says where the build is, and
// waiting again is how one goes on waiting.
func TestWaitForPauseReportsABuildThatHasNotArrived(t *testing.T) {
	service := newService(t)
	build, err := service.Start(Request{Template: template(t), Env: map[string]string{"TRIGGER": neverPath(t)}})
	require.NoError(t, err)
	t.Cleanup(func() { stop(t, service, build) })

	report, err := service.WaitForPause(t.Context(), build.Meta.ID, 300*time.Millisecond, 5)

	require.NoError(t, err)
	require.False(t, report.Paused)
	require.False(t, report.Finished)
}

func TestSendKeysTypesThroughTheMonitor(t *testing.T) {
	service := newService(t)
	build := startPaused(t, service)
	build.monitor.Reply("query-status", map[string]any{"status": "running", "running": true})

	require.NoError(t, service.SendKeys(build.Meta.ID, "ok", []string{"ret"}, 0))

	// One press per character, then one for the named key, each its own
	// connection: QEMU serves a single monitor session at a time.
	require.Equal(t, 3, typedKeys(build))
}

// Keystrokes sent to a frozen guest sit in the emulated keyboard and nothing
// on the screen moves, which reads as input that went astray.
func TestSendKeysRefusesAFrozenGuest(t *testing.T) {
	service := newService(t)
	build := startPaused(t, service)

	err := service.SendKeys(build.Meta.ID, "ok", nil, 0)

	require.ErrorContains(t, err, "paused")
	require.ErrorContains(t, err, "continue the build first")
}

func TestSendKeysNoticesAGuestThatStoppedPartWayThrough(t *testing.T) {
	service := newService(t)
	build := startPaused(t, service)
	build.monitor.Reply("query-status", map[string]any{"status": "running", "running": true})
	build.monitor.ReplyAfter("query-status", 1, map[string]any{"status": "paused", "running": false})

	err := service.SendKeys(build.Meta.ID, "ok", nil, 0)

	require.ErrorContains(t, err, "stopped reading part way through")
}

func TestSendKeysRefusesAKeyThatIsNotOne(t *testing.T) {
	service := newService(t)
	build := startPaused(t, service)

	err := service.SendKeys(build.Meta.ID, "", []string{"anykey"}, 0)

	require.ErrorContains(t, err, "anykey")
}

func TestClickMeasuresTheScreenItIsAimingAt(t *testing.T) {
	service := newService(t)
	build := startPaused(t, service)
	build.monitor.Reply("query-status", map[string]any{"status": "running", "running": true})
	build.monitor.Draw(800, 600)

	clicked, err := service.Click(build.Meta.ID, image.Pt(400, 300), "left", 1)

	require.NoError(t, err)
	require.Equal(t, 800, clicked.Screen.Width)
	require.Equal(t, 600, clicked.Screen.Height)
	require.FileExists(t, clicked.Screen.Path)

	events, ok := build.monitor.Arguments("input-send-event")["events"].([]any)
	require.True(t, ok)
	require.Equal(t, map[string]any{"type": "abs", "data": map[string]any{"axis": "x", "value": float64(0x3FFF)}}, events[0])
}

func TestClickRefusesAPointThatIsNotOnTheScreen(t *testing.T) {
	service := newService(t)
	build := startPaused(t, service)
	build.monitor.Reply("query-status", map[string]any{"status": "running", "running": true})
	build.monitor.Draw(800, 600)

	_, err := service.Click(build.Meta.ID, image.Pt(900, 300), "left", 1)

	require.ErrorContains(t, err, "800x600")
}

// startedBuild is a build whose fake QEMU the test can inspect.
type startedBuild struct {
	*Build
	monitor *qmptest.Server
}

// startPaused starts a build and holds it at a boot step pause, which is where
// the harness freezes the guest.
func startPaused(t *testing.T, service *Service) *startedBuild {
	t.Helper()

	trigger := filepath.Join(t.TempDir(), "go")
	build, err := service.Start(Request{
		Template: template(t),
		Env:      map[string]string{"TRIGGER": trigger},
	})
	require.NoError(t, err)
	t.Cleanup(func() { stop(t, service, build) })

	monitor := qmptest.StartAt(t, build.Dir.MonitorSocket())
	monitor.Reply("query-status", map[string]any{"status": "paused", "running": false})
	require.NoError(t, os.WriteFile(trigger, nil, 0o600))

	awaitBuild(t, build.Dir, "pause", func() bool { return build.Dir.State().Paused })

	return &startedBuild{Build: build, monitor: monitor}
}

// stop ends a build and waits for it, so that its supervisor is finished with
// the build directory before the directory is taken away underneath it.
func stop(t *testing.T, service *Service, build *Build) {
	t.Helper()

	if build.Dir.State().Finished {
		return
	}
	stopped, err := service.Abort(context.Background(), build.Meta.ID, 0)
	if err == nil && stopped.Finished {
		return
	}
	// A build that ended on its own is still being seen to: its supervisor
	// spends a moment collecting the last of the terminal before it says the
	// build has finished.
	if settled(build.Dir, drainAllowance) {
		return
	}
	// The tests that take the supervisor's socket away, or leave something else
	// answering on it, arrive here: the supervisor cannot be asked, and it is
	// still holding a terminal and a build. Ending the build is what brings it
	// down, and the build is named in the state file.
	kill(t, build.Dir)
}

// kill ends a build that cannot be aborted through its supervisor, and waits
// for the supervisor to notice, which is what makes it let go of the
// directory. The signal reaches the whole process group, since the stand-in
// starts processes of its own.
func kill(t *testing.T, dir *buildstate.Dir) {
	t.Helper()

	pid := dir.State().BuildPID
	if pid == 0 {
		return
	}
	if err := unix.Kill(-pid, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
		t.Errorf("could not end build %d: %s", pid, err)
		return
	}
	awaitBuild(t, dir, "finish after being killed",
		func() bool { return dir.State().Finished })
}

// orphan takes the supervisor's socket away, which is all that is left of it
// once it has died.
func orphan(t *testing.T, dir *buildstate.Dir) {
	t.Helper()

	require.NoError(t, os.Remove(dir.ControlSocket()))
}

// settled reports whether the build finished within the time given. It is the
// patient version of a look at the state file, for a caller that has somewhere
// else to go if the answer is no.
func settled(dir *buildstate.Dir, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if dir.State().Finished {
			return true
		}
		time.Sleep(tick)
	}
	return false
}

func awaitFinished(t *testing.T, dir *buildstate.Dir) {
	t.Helper()

	awaitBuild(t, dir, "finish", func() bool { return dir.State().Finished })
}

// awaitBuild waits for something the build should do, and says where the build
// actually got to if it never does. These tests run on platforms that cannot
// be reached from a laptop, where "condition never satisfied" is not enough to
// go on.
func awaitBuild(t *testing.T, dir *buildstate.Dir, what string, reached func() bool,
	within ...time.Duration) {
	t.Helper()

	waitFor := patience
	if len(within) > 0 {
		waitFor = within[0]
	}
	deadline := time.Now().Add(waitFor)
	for time.Now().Before(deadline) {
		if reached() {
			return
		}
		time.Sleep(tick)
	}
	t.Fatalf("the build did not %s within %s\nstate: %+v\nlog:\n%s",
		what, waitFor, dir.State(), strings.Join(dir.TailLog(30), "\n"))
}

func newService(t *testing.T) *Service {
	t.Helper()

	root, err := os.MkdirTemp("/tmp", "svc")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(root) })

	service := New(buildstate.New(root), fakePacker(t))
	service.launcher = launcherFunc(func(dir *buildstate.Dir) error {
		go supervisor.Run(dir, supervisor.ShutdownGrace(shutdownGrace))
		return nil
	})
	service.startup = patience
	service.settle = 50 * time.Millisecond
	service.stopWait = shutdownGrace + patience
	return service
}

// fakePacker stands in for the packer binary: it waits to be let go, prints
// the pause prompt of a boot step, and waits for an answer.
func fakePacker(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "fake-packer")
	require.NoError(t, os.WriteFile(path, []byte(`#!/bin/sh
trap 'exit 1' INT TERM
while [ ! -e "$TRIGGER" ]; do sleep 1; done
`+packertest.BootStepPause("boot menu")+`
printf "\n==> qemu.probe: carried on\n"
`), 0o700))
	return path
}

// typedKeys counts the presses the fake monitor was sent.
func typedKeys(build *startedBuild) int {
	sent := 0
	for _, executed := range build.monitor.Executed() {
		if executed == "send-key" {
			sent++
		}
	}
	return sent
}

// yesterdaysBuild is a build started before today and left in the root the way
// a real one is: nothing tidies these up.
func yesterdaysBuild(t *testing.T, service *Service, state buildstate.State) string {
	t.Helper()

	dir, err := service.root.Create("stale-1")
	require.NoError(t, err)
	require.NoError(t, dir.WriteMeta(buildstate.Meta{
		ID:        dir.ID(),
		Template:  "old.pkr.hcl",
		StartedAt: time.Now().UTC().Add(-48 * time.Hour),
	}))
	require.NoError(t, dir.PublishState(state))
	return dir.ID()
}

// twicePausingPacker stands in for a build with a second boot step to reach,
// which is what a wait for a pause is for.
func twicePausingPacker(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "twice-pausing-packer")
	require.NoError(t, os.WriteFile(path, []byte(`#!/bin/sh
trap 'exit 1' INT TERM
while [ ! -e "$TRIGGER" ]; do sleep 1; done
`+packertest.BootStepPause("boot menu")+`
printf "\n"
`+packertest.BootStepPause("second thoughts")+`
printf "\n==> qemu.probe: carried on\n"
`), 0o700))
	return path
}

// refusingPacker stands in for a Packer whose virtual machine will not start:
// it says so on the terminal in the unhelpful way Packer does, and puts the
// reason where Packer puts it, in its own log.
func refusingPacker(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "refusing-packer")
	require.NoError(t, os.WriteFile(path, []byte(`#!/bin/sh
printf "qemu.probe: Error launching VM: Qemu failed to start. Please run with PACKER_LOG=1
"
printf "[INFO] Qemu stderr: qemu-system-x86_64: -display gtk: unsupported display
" >> "$PACKER_LOG_PATH"
exit 1
`), 0o700))
	return path
}

func template(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "probe.pkr.hcl")
	require.NoError(t, os.WriteFile(path, []byte("# a template\n"), 0o600))
	return path
}

func neverPath(t *testing.T) string {
	t.Helper()

	return filepath.Join(t.TempDir(), "never")
}

type launcherFunc func(*buildstate.Dir) error

func (f launcherFunc) Launch(dir *buildstate.Dir) error { return f(dir) }

func summaryIDs(summaries []Summary) []string {
	ids := make([]string, 0, len(summaries))
	for _, summary := range summaries {
		ids = append(ids, summary.ID)
	}
	return ids
}
