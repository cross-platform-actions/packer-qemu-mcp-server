// Package buildstate is the on-disk record of detached builds.
//
// A build outlives the processes that watch it: it is started detached, and
// the server that started it can be restarted or replaced without the build
// noticing. Everything needed to find a build again therefore lives in a
// directory on disk rather than in any process's memory.
package buildstate

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// DefaultRoot is where build directories live unless told otherwise. It is
// deliberately shallow: unix socket paths are limited to little more than a
// hundred bytes, and both the monitor and the control socket live inside a
// build's directory.
const DefaultRoot = "/tmp/pk-builds"

// socketPathLimit is the shortest sun_path limit among the systems this runs
// on (104 on macOS, 108 on Linux). The limit counts the terminating NUL, so a
// path of exactly that many bytes is already one too long.
const socketPathLimit = 104

// logTail bounds how much of a build's log is read to answer a status request.
const logTail = 64 * 1024

// Meta is what a build was asked to do. It is written once, when the build
// starts.
type Meta struct {
	ID         string            `json:"id"`
	Template   string            `json:"template"`
	WorkingDir string            `json:"working_dir"`
	Command    []string          `json:"command"`
	Env        map[string]string `json:"env,omitempty"`
	StartedAt  time.Time         `json:"started_at"`
	// Freeze says whether the guest is stopped while the build waits at a
	// pause. It is a pointer because an absent field must mean the freezing
	// this harness is for, not the silent absence of it.
	Freeze *bool `json:"freeze,omitempty"`
	// Settle is how long the guest is left running after a pause before it is
	// frozen, for finding out whether stopping it so promptly is what disturbs
	// the keystrokes of the step that has just been typed.
	Settle time.Duration `json:"settle,omitempty"`
	// Monitored says whether the build was given a monitor socket at all. A
	// template that opens none can never be photographed or frozen, which is a
	// different thing from an emulator that has not started yet.
	Monitored bool `json:"monitored,omitempty"`
}

// Freezing reports whether this build stops its guest at a pause.
func (m Meta) Freezing() bool { return m.Freeze == nil || *m.Freeze }

// State is what a build is doing. The supervisor republishes it on every
// transition so that anyone who reads the directory — including a server that
// was not running when the build started — learns where the build got to.
type State struct {
	SupervisorPID int `json:"supervisor_pid,omitempty"`
	BuildPID      int `json:"build_pid,omitempty"`
	// Step and Label are the last pause the build reached, not the step it is
	// running now. Under -debug a step is announced when it finishes, so a
	// build is always somewhere past the pause it last answered; the log is
	// the live view.
	Step   string `json:"last_pause_step,omitempty"`
	Label  string `json:"last_pause,omitempty"`
	Paused bool   `json:"paused"`
	// Awaiting is the step the build is being run forward to, if any. While it
	// is set, boot steps are passed through rather than stopped at.
	Awaiting string `json:"awaiting,omitempty"`
	Frozen   bool   `json:"frozen"`
	// Screenshot is the capture taken at the pause the build is waiting at,
	// and ScreenshotLabel is the step it belongs to. Both are dropped the
	// moment the build moves on: a picture of the screen at a step that
	// finished minutes ago, offered as the current one, is worse than no
	// picture at all.
	Screenshot      string `json:"screenshot,omitempty"`
	ScreenshotLabel string `json:"screenshot_label,omitempty"`
	// Trail is the boot steps the build has been run past since it was last
	// let go, photographed but not stopped at. A desync starts at one step and
	// is noticed at another, and without this the fifty steps in between are
	// gone by the time anyone looks.
	Trail      []Frame   `json:"trail,omitempty"`
	Note       string    `json:"note,omitempty"`
	Finished   bool      `json:"finished"`
	ExitCode   int       `json:"exit_code,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// A Frame is one boot step the build went past, and the screen as that step
// finished.
type Frame struct {
	Label string `json:"label"`
	// Screenshot is empty when the guest could not be photographed, which is
	// worth seeing: a gap in the trail is itself something to explain.
	Screenshot string `json:"screenshot,omitempty"`
}

// Root is the directory holding every build's directory.
type Root struct {
	path string
}

// New returns the root at the given path.
func New(path string) *Root {
	return &Root{path: path}
}

// Path is the root directory.
func (r *Root) Path() string { return r.path }

// Create makes the directory for a new build.
func (r *Root) Create(id string) (*Dir, error) {
	if err := validID(id); err != nil {
		return nil, err
	}
	dir := &Dir{path: filepath.Join(r.path, id), id: id}
	if err := dir.checkSocketPaths(); err != nil {
		return nil, err
	}
	if err := r.prepare(); err != nil {
		return nil, err
	}
	if err := os.Mkdir(dir.path, 0o700); err != nil {
		return nil, fmt.Errorf("creating build directory for %q: %w", id, err)
	}
	if err := os.Mkdir(dir.ScreenshotDir(), 0o700); err != nil {
		return nil, fmt.Errorf("creating screenshot directory for %q: %w", id, err)
	}
	return dir, nil
}

// prepare makes sure the root exists and belongs to us.
//
// The default root sits in a world-writable directory, where anyone on the
// host can create it first. Building inside a directory somebody else owns
// would hand them the control socket — and with it the ability to answer a
// build's pauses, type at its virtual machine, and stop it.
func (r *Root) prepare() error {
	info, err := os.Stat(r.path)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(r.path, 0o700); err != nil {
			return fmt.Errorf("creating build root: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading build root: %w", err)
	}

	if !info.IsDir() {
		return fmt.Errorf("build root %s is not a directory", r.path)
	}
	if owner, ok := info.Sys().(*syscall.Stat_t); ok && int(owner.Uid) != os.Getuid() {
		return fmt.Errorf("build root %s belongs to uid %d, not to you", r.path, owner.Uid)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("build root %s is readable or writable by others (mode %#o); "+
			"a build's control socket lives in it", r.path, info.Mode().Perm())
	}
	return nil
}

// Open returns an existing build's directory.
func (r *Root) Open(id string) (*Dir, error) {
	if err := validID(id); err != nil {
		return nil, err
	}
	dir := &Dir{path: filepath.Join(r.path, id), id: id}
	if _, err := os.Stat(dir.path); err != nil {
		return nil, fmt.Errorf("no such build %q", id)
	}
	return dir, nil
}

// List returns every build under the root, most recently started first. A root
// that does not exist yet holds no builds; that is not an error.
func (r *Root) List() ([]*Dir, error) {
	entries, err := os.ReadDir(r.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading build root: %w", err)
	}

	var dirs []*Dir
	started := map[string]time.Time{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := &Dir{path: filepath.Join(r.path, entry.Name()), id: entry.Name()}
		dirs = append(dirs, dir)
		// Read once: a comparison function that reads a file is a comparison
		// function that reads it O(n log n) times.
		started[dir.ID()] = dir.Meta().StartedAt
	}
	sort.Slice(dirs, func(i, j int) bool {
		return started[dirs[i].ID()].After(started[dirs[j].ID()])
	})
	return dirs, nil
}

// Dir is one build's state directory.
type Dir struct {
	path string
	id   string
}

// At returns the build directory at a path, for callers that were handed one —
// a supervisor, for instance, which is told which build it is supervising and
// nothing else.
func At(path string) *Dir {
	return &Dir{path: path, id: filepath.Base(path)}
}

// ID is the build's identifier.
func (d *Dir) ID() string { return d.id }

// Path is the build's directory.
func (d *Dir) Path() string { return d.path }

// MonitorSocket is where QEMU is asked to listen for monitor connections.
func (d *Dir) MonitorSocket() string { return filepath.Join(d.path, "mon.sock") }

// ControlSocket is where the build's supervisor listens.
func (d *Dir) ControlSocket() string { return filepath.Join(d.path, "ctl.sock") }

// LogPath is everything the build's terminal produced.
func (d *Dir) LogPath() string { return filepath.Join(d.path, "log") }

// SupervisorLogPath is where the supervisor's own complaints go. It is the
// first place to look when a build never starts.
func (d *Dir) SupervisorLogPath() string { return filepath.Join(d.path, "supervisor.log") }

// PackerLogPath is where Packer's own log goes. Keeping it out of the terminal
// matters: Packer echoes its prompts into that log, and a second copy of a
// pause prompt on the terminal would be answered twice.
func (d *Dir) PackerLogPath() string { return filepath.Join(d.path, "packer.log") }

// ScreenshotDir holds the framebuffer captures. QEMU writes them itself, so it
// must be writable by whoever runs the build.
func (d *Dir) ScreenshotDir() string { return filepath.Join(d.path, "shots") }

// NewScreenshotPath names the next capture, sortable by when it was taken.
//
// The clock is not fine-grained enough to separate two captures on its own —
// the supervisor photographing a pause and an agent asking for a picture of
// the same moment are microseconds apart — and one overwriting the other
// would quietly show the wrong screen.
func (d *Dir) NewScreenshotPath() string {
	name := fmt.Sprintf("%s-%s.png", time.Now().UTC().Format("20060102-150405.000000"), randomSuffix())
	return filepath.Join(d.ScreenshotDir(), name)
}

// WriteMeta records what the build was asked to do.
func (d *Dir) WriteMeta(meta Meta) error {
	return d.writeJSON("meta.json", meta)
}

// Meta is what the build was asked to do. A build whose directory is
// unreadable or half-written reads as empty rather than as an error: a status
// report is more useful than a failure.
func (d *Dir) Meta() Meta {
	var meta Meta
	d.readJSON("meta.json", &meta)
	return meta
}

// PublishState records what the build is doing now.
func (d *Dir) PublishState(state State) error {
	state.UpdatedAt = time.Now().UTC()
	return d.writeJSON("state.json", state)
}

// State is what the build was last seen doing.
func (d *Dir) State() State {
	var state State
	d.readJSON("state.json", &state)
	return state
}

// TailLog returns the last few lines of the build's terminal output, including
// a pause prompt still waiting for an answer.
func (d *Dir) TailLog(lines int) []string {
	return d.tail(d.LogPath(), lines)
}

// TailPackerLog returns the last few lines of Packer's own log, which is where
// QEMU's complaints end up: the plugin logs everything the emulator writes to
// its standard error, and none of it reaches the terminal.
func (d *Dir) TailPackerLog(lines int) []string {
	return d.tail(d.PackerLogPath(), lines)
}

func (d *Dir) tail(path string, lines int) []string {
	if lines <= 0 {
		return nil
	}

	text, err := d.readTail(path)
	if err != nil {
		return nil
	}

	all := strings.Split(text, "\n")
	for i, line := range all {
		all[i] = strings.TrimRight(line, " \t\r")
	}
	if last := len(all) - 1; last >= 0 && all[last] == "" {
		all = all[:last]
	}
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return all
}

func (d *Dir) readTail(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return "", err
	}

	size := min(info.Size(), logTail)
	buffer := make([]byte, size)
	if _, err := file.ReadAt(buffer, info.Size()-size); err != nil {
		return "", err
	}
	if size < info.Size() {
		// The first line is probably half a line.
		if cut := strings.IndexByte(string(buffer), '\n'); cut >= 0 {
			return string(buffer[cut+1:]), nil
		}
	}
	return string(buffer), nil
}

// writeJSON replaces a file in one step, so that a reader either sees the
// previous contents or the new ones and never a half-written file.
func (d *Dir) writeJSON(name string, value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding %s: %w", name, err)
	}

	temporary := filepath.Join(d.path, "."+name+".new")
	if err := os.WriteFile(temporary, append(encoded, '\n'), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}
	if err := os.Rename(temporary, filepath.Join(d.path, name)); err != nil {
		return fmt.Errorf("replacing %s: %w", name, err)
	}
	return nil
}

func (d *Dir) readJSON(name string, value any) {
	encoded, err := os.ReadFile(filepath.Join(d.path, name))
	if err != nil {
		return
	}
	// A file that cannot be read back leaves the value as it was, which is
	// the same answer as a file that is not there yet.
	_ = json.Unmarshal(encoded, value)
}

func (d *Dir) checkSocketPaths() error {
	for _, socket := range []string{d.MonitorSocket(), d.ControlSocket()} {
		if len(socket) >= socketPathLimit {
			return fmt.Errorf("build directory is too long for a unix socket: %s is %d bytes, the limit is %d",
				socket, len(socket), socketPathLimit-1)
		}
	}
	return nil
}

var (
	idRE       = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
	notSlugRE  = regexp.MustCompile(`[^a-z0-9]+`)
	extensions = []string{".pkr.hcl", ".pkr.json", ".hcl", ".json"}
)

// maxSlug bounds the readable half of an identifier.
const maxSlug = 24

// NewID names a build after its template, with enough of a suffix that two
// builds of the same template started at once cannot collide.
func NewID(template string) string {
	name := strings.ToLower(filepath.Base(template))
	for _, extension := range extensions {
		name = strings.TrimSuffix(name, extension)
	}

	slug := strings.Trim(notSlugRE.ReplaceAllString(name, "-"), "-")
	if slug == "" {
		slug = "build"
	}
	if len(slug) > maxSlug {
		slug = strings.Trim(slug[:maxSlug], "-")
	}

	return fmt.Sprintf("%s-%s-%s", slug, time.Now().UTC().Format("20060102-150405"), randomSuffix())
}

// randomSuffix is enough randomness to keep two names apart, and no more.
func randomSuffix() string {
	suffix := make([]byte, 3)
	rand.Read(suffix)
	return hex.EncodeToString(suffix)
}

func validID(id string) error {
	if !idRE.MatchString(id) {
		return fmt.Errorf("invalid build id %q", id)
	}
	return nil
}
