package builds

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/cross-platform-actions/packer-qemu-mcp-server/internal/buildstate"
)

func TestLaunchStartsTheSupervisorForTheBuild(t *testing.T) {
	dir, launcher, recorded := launch(t)

	require.NoError(t, launcher.Launch(dir))

	require.Equal(t, []string{"supervise", "--dir", dir.Path()}, arguments(t, recorded))
}

func TestLaunchDoesNotWaitForTheSupervisor(t *testing.T) {
	dir, launcher, _ := launch(t)

	started := time.Now()
	require.NoError(t, launcher.Launch(dir))

	// A build runs for hours. Nothing may wait on it: this has to return well
	// inside the minute the stand-in supervisor sleeps for, with enough slack
	// for a slow emulated guest to have got there at all.
	require.Less(t, time.Since(started), 10*time.Second)
}

func TestTheSupervisorGetsASessionOfItsOwn(t *testing.T) {
	dir, launcher, recorded := launch(t)

	require.NoError(t, launcher.Launch(dir))

	// Detached from this process's terminal and process group, so that
	// whatever happens to the server the build carries on.
	supervisorGroup, err := unix.Getpgid(pid(t, recorded))
	if errors.Is(err, unix.EPERM) {
		// OpenBSD will not answer for a process outside the caller's session,
		// and being refused is the same answer: it is not in ours.
		return
	}
	require.NoError(t, err)
	ourGroup, err := unix.Getpgid(os.Getpid())
	require.NoError(t, err)
	require.NotEqual(t, ourGroup, supervisorGroup)
}

func TestLaunchKeepsWhatTheSupervisorSays(t *testing.T) {
	dir, launcher, _ := launch(t)

	require.NoError(t, launcher.Launch(dir))

	require.Eventually(t, func() bool {
		said, err := os.ReadFile(dir.SupervisorLogPath())
		return err == nil && strings.Contains(string(said), "supervising")
	}, patience, tick)
}

// launch returns a build directory and a launcher wired to a stand-in for the
// supervisor, which records how it was started.
func launch(t *testing.T) (*buildstate.Dir, *DetachedLauncher, string) {
	t.Helper()

	root, err := os.MkdirTemp("/tmp", "launch")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(root) })

	dir, err := buildstate.New(root).Create("build-1")
	require.NoError(t, err)

	recorded := filepath.Join(root, "recorded")
	fake := filepath.Join(root, "fake-supervisor")
	require.NoError(t, os.WriteFile(fake, []byte(`#!/bin/sh
echo "$$" > `+recorded+`.pid
echo "$@" > `+recorded+`.args
echo supervising
sleep 60
`), 0o700))

	t.Cleanup(func() { stopStandIn(recorded) })
	return dir, &DetachedLauncher{Executable: fake}, recorded
}

// stopStandIn ends the stand-in supervisor and the minute-long sleep it
// started, which would otherwise outlive the test binary. It is given a
// session of its own to be detached, so signalling its process group reaches
// both.
//
// A test that failed before the stand-in wrote its pid has nothing to end, and
// cleanup is no place to wait for one or to fail: failing here would skip
// every cleanup still to run, and leave the directory behind that one of them
// removes.
func stopStandIn(recorded string) {
	written, err := os.ReadFile(recorded + ".pid")
	if err != nil {
		return
	}
	leader, err := strconv.Atoi(strings.TrimSpace(string(written)))
	if err != nil {
		return
	}
	_ = unix.Kill(-leader, unix.SIGKILL)
}

func arguments(t *testing.T, recorded string) []string {
	t.Helper()

	return strings.Fields(strings.TrimSpace(read(t, recorded+".args")))
}

func pid(t *testing.T, recorded string) int {
	t.Helper()

	number, err := strconv.Atoi(strings.TrimSpace(read(t, recorded+".pid")))
	require.NoError(t, err)
	return number
}

func read(t *testing.T, path string) string {
	t.Helper()

	var contents []byte
	require.Eventually(t, func() bool {
		var err error
		contents, err = os.ReadFile(path)
		return err == nil && len(contents) > 0
	}, patience, tick, "the supervisor never wrote %s", path)
	return string(contents)
}
