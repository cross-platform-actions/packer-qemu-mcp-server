package buildstate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCreateLaysOutTheBuildDirectory(t *testing.T) {
	root := testRoot(t)

	dir, err := root.Create("ubuntu-1")

	require.NoError(t, err)
	require.DirExists(t, dir.Path())
	require.DirExists(t, dir.ScreenshotDir())
	require.Equal(t, filepath.Join(dir.Path(), "mon.sock"), dir.MonitorSocket())
	require.Equal(t, filepath.Join(dir.Path(), "ctl.sock"), dir.ControlSocket())
	require.Equal(t, filepath.Join(dir.Path(), "log"), dir.LogPath())
	require.Equal(t, filepath.Join(dir.Path(), "packer.log"), dir.PackerLogPath())
}

func TestCreatedDirectoryIsPrivate(t *testing.T) {
	root := testRoot(t)

	dir, err := root.Create("ubuntu-1")

	require.NoError(t, err)
	info, err := os.Stat(dir.Path())
	require.NoError(t, err)
	// A control socket that can inject keystrokes into a virtual machine has
	// no business being reachable by other users on the host.
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

func TestCreateRefusesARootOthersCanWriteTo(t *testing.T) {
	// The default root lives in a world-writable directory, so somebody else
	// can get there first; a build inside a directory they own is a build they
	// can type at.
	path, err := os.MkdirTemp("/tmp", "bs")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(path) })
	require.NoError(t, os.Chmod(path, 0o777))

	_, err = New(path).Create("ubuntu-1")

	require.ErrorContains(t, err, "writable by others")
}

func TestCreateRefusesToReuseAnID(t *testing.T) {
	root := testRoot(t)
	_, err := root.Create("ubuntu-1")
	require.NoError(t, err)

	_, err = root.Create("ubuntu-1")

	require.ErrorContains(t, err, "ubuntu-1")
}

func TestCreateRefusesAnIDThatOverflowsTheSocketPath(t *testing.T) {
	root := testRoot(t)

	_, err := root.Create(strings.Repeat("x", 100))

	require.ErrorContains(t, err, "too long")
}

func TestIDsAreDerivedFromTheTemplateAndAreUnique(t *testing.T) {
	first := NewID("/home/me/templates/Ubuntu 24.04.pkr.hcl")
	second := NewID("/home/me/templates/Ubuntu 24.04.pkr.hcl")

	require.True(t, strings.HasPrefix(first, "ubuntu-24-04-"), first)
	require.NotEqual(t, first, second)
	require.NotContains(t, first, "/")
}

func TestADirectoryCanBeOpenedByPathAlone(t *testing.T) {
	created := testDir(t)

	opened := At(created.Path())

	require.Equal(t, created.ID(), opened.ID())
	require.Equal(t, created.MonitorSocket(), opened.MonitorSocket())
}

func TestMetaRoundTrips(t *testing.T) {
	dir := testDir(t)
	meta := Meta{
		ID:         "ubuntu-1",
		Template:   "ubuntu.pkr.hcl",
		WorkingDir: "/tmp",
		Command:    []string{"packer", "build", "-debug", "ubuntu.pkr.hcl"},
		StartedAt:  time.Now().UTC().Truncate(time.Second),
	}

	require.NoError(t, dir.WriteMeta(meta))

	require.Equal(t, meta, dir.Meta())
}

func TestStateRoundTrips(t *testing.T) {
	dir := testDir(t)
	state := State{Step: "stepTypeBootCommand", Label: "boot menu", Paused: true, Frozen: true}

	require.NoError(t, dir.PublishState(state))

	published := dir.State()
	require.Equal(t, state.Step, published.Step)
	require.True(t, published.Paused)
	require.True(t, published.Frozen)
	require.False(t, published.UpdatedAt.IsZero())
}

func TestStateIsPublishedAtomically(t *testing.T) {
	dir := testDir(t)

	require.NoError(t, dir.PublishState(State{Step: "stepRun"}))
	require.NoError(t, dir.PublishState(State{Step: "stepConnect"}))

	entries, err := os.ReadDir(dir.Path())
	require.NoError(t, err)
	for _, entry := range entries {
		require.NotContains(t, entry.Name(), "tmp", "half-written state left behind")
	}
	require.Equal(t, "stepConnect", dir.State().Step)
}

func TestUnwrittenStateReadsAsEmpty(t *testing.T) {
	dir := testDir(t)

	require.Equal(t, State{}, dir.State())
	require.Equal(t, Meta{}, dir.Meta())
}

func TestListReturnsTheNewestBuildFirst(t *testing.T) {
	root := testRoot(t)
	older, err := root.Create("older")
	require.NoError(t, err)
	require.NoError(t, older.WriteMeta(Meta{ID: "older", StartedAt: time.Now().Add(-time.Hour)}))
	newer, err := root.Create("newer")
	require.NoError(t, err)
	require.NoError(t, newer.WriteMeta(Meta{ID: "newer", StartedAt: time.Now()}))

	dirs, err := root.List()

	require.NoError(t, err)
	require.Equal(t, []string{"newer", "older"}, ids(dirs))
}

func TestListIgnoresStrayFiles(t *testing.T) {
	root := testRoot(t)
	require.NoError(t, os.WriteFile(filepath.Join(root.Path(), "notes.txt"), nil, 0o600))

	dirs, err := root.List()

	require.NoError(t, err)
	require.Empty(t, dirs)
}

func TestListOfAMissingRootIsEmpty(t *testing.T) {
	root := New(filepath.Join(t.TempDir(), "never-created"))

	dirs, err := root.List()

	require.NoError(t, err)
	require.Empty(t, dirs)
}

func TestOpenRejectsAnUnknownBuild(t *testing.T) {
	root := testRoot(t)

	_, err := root.Open("nope")

	require.ErrorContains(t, err, "nope")
}

func TestOpenRejectsAnIDThatEscapesTheRoot(t *testing.T) {
	root := testRoot(t)

	_, err := root.Open("../../etc")

	require.ErrorContains(t, err, "invalid")
}

func TestTailLogReturnsTheLastLines(t *testing.T) {
	dir := testDir(t)
	// A build's terminal ends lines with a carriage return, which no reader of
	// a status report wants to see.
	require.NoError(t, os.WriteFile(dir.LogPath(), []byte("one\r\ntwo\r\nthree\r\n"), 0o600))

	require.Equal(t, []string{"two", "three"}, dir.TailLog(2))
	require.Equal(t, []string{"one", "two", "three"}, dir.TailLog(10))
}

func TestTailLogOfAnUnstartedBuildIsEmpty(t *testing.T) {
	dir := testDir(t)

	require.Empty(t, dir.TailLog(10))
}

func TestTailLogKeepsAnUnfinishedPrompt(t *testing.T) {
	dir := testDir(t)
	// The pause prompt is the most interesting line in the log and it never
	// ends with a newline.
	require.NoError(t, os.WriteFile(dir.LogPath(), []byte("one\r\nPausing after run of step 'stepRun'. "), 0o600))

	require.Equal(t, []string{"one", "Pausing after run of step 'stepRun'."}, dir.TailLog(2))
}

func TestScreenshotPathsAreUnique(t *testing.T) {
	dir := testDir(t)

	first := dir.NewScreenshotPath()
	second := dir.NewScreenshotPath()

	require.NotEqual(t, first, second)
	require.Equal(t, dir.ScreenshotDir(), filepath.Dir(first))
	require.Equal(t, ".png", filepath.Ext(first))
}

func testRoot(t *testing.T) *Root {
	t.Helper()

	// Unix socket paths are limited to about a hundred bytes, which the
	// default temporary directory on macOS is far too deep for.
	path, err := os.MkdirTemp("/tmp", "bs")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(path) })
	return New(path)
}

func testDir(t *testing.T) *Dir {
	t.Helper()

	dir, err := testRoot(t).Create("ubuntu-1")
	require.NoError(t, err)
	return dir
}

func ids(dirs []*Dir) []string {
	names := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		names = append(names, dir.ID())
	}
	return names
}
