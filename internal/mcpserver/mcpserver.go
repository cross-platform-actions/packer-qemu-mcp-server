// Package mcpserver exposes the build tools over MCP.
//
// It is only an adapter: every tool here is one call into the build service,
// and the descriptions exist to teach an agent the two rules it cannot infer
// from the schema — that the guest is frozen for it while a build waits at a
// boot step, and that a screenshot is possible at any time, not only then.
package mcpserver

import (
	"context"
	"fmt"
	"image"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cross-platform-actions/packer-qemu-mcp-server/internal/builds"
)

// defaultLogLines is how much of a build's terminal a status report carries
// unless asked for more.
const defaultLogLines = 40

// defaultWait is how long a wait for a pause lasts unless asked for more, and
// maximumWait is as long as one may be asked for. The ceiling is there because
// a tool call that never returns is a session that cannot be steered.
const (
	defaultWait = 5 * time.Minute
	maximumWait = time.Hour
)

// defaultButton is the mouse button a click uses unless another is named.
const defaultButton = "left"

// serverName is how this server introduces itself to a client.
const serverName = "packer-qemu-mcp-server"

// Builds is the service the tools are a surface for.
type Builds interface {
	Start(request builds.Request) (*builds.Build, error)
	List(all bool) (*builds.Listing, error)
	Status(id string, logLines int) (*builds.Report, error)
	WaitForPause(ctx context.Context, id string, within time.Duration, logLines int) (*builds.Report, error)
	Screenshot(id string) (string, error)
	SendKeys(id, text string, keys []string, hold time.Duration) error
	Click(id string, at image.Point, button string, times int) (*builds.Clicked, error)
	Continue(id, untilLabel string) error
	Abort(ctx context.Context, id string, logLines int) (*builds.Report, error)
}

// New returns a server offering the build tools.
func New(service Builds, version string) *mcp.Server {
	// The name a client shows and configures this server under. It matches the
	// binary and the repository, as terraform-mcp-server does.
	server := mcp.NewServer(&mcp.Implementation{Name: serverName, Version: version}, nil)
	tools := &tools{builds: service}

	mcp.AddTool(server, &mcp.Tool{
		Name: "start_build",
		Description: strings.TrimSpace(`
Start a Packer build of a QEMU template, detached, and return its build id.

The build outlives this server: it keeps running across a reload or a crash,
and is found again through list_builds. Under -debug, which is on by default,
the harness answers the pauses nobody wants to step through — downloading an
ISO, creating a disk — and stops at each entry of boot_steps, freezing the
guest while it waits so that no boot menu counts down while you think.

The template must open a monitor socket of its own, because Packer holds the
one it opens itself:

  variable "qmp_socket" { type = string }
  qemuargs = [["-qmp", "unix:${var.qmp_socket},server=on,wait=off"]]

The path is passed as the qmp_socket variable; monitor_var renames it, and an
empty monitor_var passes nothing. Without it there are no screenshots.

Add -device usb-tablet to qemuargs if you want to click at the guest: the
default PS/2 mouse reports movements, and click needs a device that reports a
position.

Call status next: a template Packer refuses will show up there, along with the
path of Packer's own log, which is the only place QEMU's own messages appear.`),
	}, tools.start)

	mcp.AddTool(server, &mcp.Tool{
		Name: "list_builds",
		Description: strings.TrimSpace(`
List the builds on this host, most recently started first, including builds
started by an earlier session of this server.

Builds that finished more than a day ago are left out unless all is set: a
host keeps every build it has ever run.`),
	}, tools.list)

	mcp.AddTool(server, &mcp.Tool{
		Name: "status",
		Description: strings.TrimSpace(`
Report where a build got to: the last pause it reached, whether it is waiting
there or running forward to a named step, what the emulator says about the
guest, and the last lines of its terminal.

last_pause is the pause the build last answered, not the step it is running
now: under -debug a step is announced as it finishes, so the build is always
somewhere past it. The log is the live view.

guest is the emulator's own word — running, paused, not started before Packer
has started the virtual machine, gone once it has stopped, busy if something
else is holding the monitor for a moment.

packer_log is the path of Packer's log. QEMU's messages go there and nowhere
else, so a virtual machine that refuses to start is explained in that file and
in no other.

A build that is paused stays paused, and its guest stays frozen, until you
call continue or abort.`),
	}, tools.status)

	mcp.AddTool(server, &mcp.Tool{
		Name: "wait_for_pause",
		Description: strings.TrimSpace(`
Wait until a build stops at a pause, or finishes, and report where it got to.

This is how to sit out the long stretches — an installer copying files for ten
minutes, a step that ends in <wait60s> — without spending a turn on each poll
of status. Running out of time is not an error: the report says whether the
build is paused, so waiting again is the way to keep waiting.`),
	}, tools.waitForPause)

	mcp.AddTool(server, &mcp.Tool{
		Name: "screenshot",
		Description: strings.TrimSpace(`
Photograph the guest's screen and return the path of a PNG file to read.

At a boot step pause this is the screen as that step finished. Usually it
already shows what the step's keystrokes did: Packer leaves one key interval
(100ms by default) after the last keystroke before it announces the pause, and
a guest that is keeping up has redrawn by then. On a slow or busy guest it may
instead be the screen those keystrokes landed on. Read it as the end of the
step, and do not assume either way — settle_ms at start_build widens the gap
if a guest is consistently caught mid-redraw.

This works whenever the virtual machine is running, not only at a pause, and
it is the main way to see an installation that is under way: under emulation
the installation happens inside a single Packer step, waiting for SSH, where
-debug offers no pause at all. Taking a screenshot never changes whether the
guest is running.

It fails before Packer has started the virtual machine, and on a template with
no graphics device, where there is no framebuffer to read.`),
	}, tools.screenshot)

	mcp.AddTool(server, &mcp.Tool{
		Name: "send_keys",
		Description: strings.TrimSpace(`
Type at the guest, through the emulated keyboard.

This asks a question of a running installation without rebuilding it: open a
console and run a command, answer a prompt, escape a menu that went the wrong
way. Nothing has to be running inside the guest for it to work, and it works
where nothing else can be asked anything — a boot loader, an installer, a
kernel that has not finished booting.

text is typed character by character on a US keyboard. keys are named keys and
combinations, sent after the text: "down", "ret", "ctrl+alt+f2", "f12". Give
either, or both.

The guest has to be running: a frozen guest reads nothing, so continue the
build first. This types into whatever has keyboard focus, exactly as a person
at the machine would, and the guest cannot tell the difference — including
that it may confuse a boot command Packer is in the middle of typing.`),
	}, tools.sendKeys)

	mcp.AddTool(server, &mcp.Tool{
		Name: "click",
		Description: strings.TrimSpace(`
Press a mouse button at a point on the guest's screen, in pixels of the
screenshot the point was read off.

The screen is photographed first, both to measure it and to leave a record of
what was clicked on; the capture is returned along with its size, so a click
that missed can be seen to have missed.

This needs a template with an absolute pointing device — -device usb-tablet in
qemuargs, or the virtio equivalent. The default PS/2 mouse reports movements
rather than positions and cannot be told to go anywhere.`),
	}, tools.click)

	mcp.AddTool(server, &mcp.Tool{
		Name: "continue",
		Description: strings.TrimSpace(`
Let a paused build carry on, resuming the guest first so that the keystrokes
Packer is about to type reach a guest that can consume them.

until_label runs the build forward to a named boot step instead of stopping at
the next one, answering every pause on the way and freezing at the one you
named. An installer's boot command can be sixty steps long, so this is how you
get to the step you care about. Matching is on any part of the step's label,
ignoring case. If no step matches, the build stops at the end of the boot
command rather than running on into the wait for SSH, and says so.

Every boot step passed on the way is photographed, without stopping at it, and
status reports the trail. A desync begins at one step and is noticed at
another, so that trail is how you find which of the fifty skipped steps went
wrong rather than only that something did.

This returns as soon as the build is moving, and fails if the build is not at
a pause to be let go of — routine while a step that ends in <wait30s> is still
running. Use wait_for_pause to be told when it arrives rather than polling for
it.`),
	}, tools.resume)

	mcp.AddTool(server, &mcp.Tool{
		Name: "abort",
		Description: strings.TrimSpace(`
Stop a build, resuming its guest first so that no virtual machine is left
frozen, and report where it had got to.

This waits for the build to be gone before returning. Packer deletes its
output directory on the way out, and a build started into that directory while
the previous one is still deleting it fails to launch, complaining about QEMU
rather than about the race it lost.`),
	}, tools.abort)

	return server
}

type tools struct {
	builds Builds
}

// StartInput is a build to start.
type StartInput struct {
	Template   string            `json:"template" jsonschema:"path to the Packer template to build"`
	Vars       map[string]string `json:"vars,omitempty" jsonschema:"Packer variables, passed as -var name=value"`
	VarFiles   []string          `json:"var_files,omitempty" jsonschema:"Packer variable files, passed as -var-file, in the order given"`
	WorkingDir string            `json:"working_dir,omitempty" jsonschema:"directory to run Packer in, defaulting to the template's own directory"`
	Env        map[string]string `json:"env,omitempty" jsonschema:"extra environment variables for the build, such as PACKER_KEY_INTERVAL"`
	ExtraArgs  []string          `json:"extra_args,omitempty" jsonschema:"further arguments for packer build, such as -only or -on-error=ask"`
	Debug      *bool             `json:"debug,omitempty" jsonschema:"run under -debug so the build can be stepped through; on unless set to false"`
	MonitorVar *string           `json:"monitor_var,omitempty" jsonschema:"template variable that receives the monitor socket path; defaults to qmp_socket, and an empty name passes none"`
	Freeze     *bool             `json:"freeze,omitempty" jsonschema:"freeze the guest while the build waits at a pause; on unless set to false"`
	SettleMs   int               `json:"settle_ms,omitempty" jsonschema:"how long to let the guest run after a pause before freezing it, so that it has taken in the keystrokes of the step just typed; a second at most"`
}

// StartOutput is the build that was started.
type StartOutput struct {
	BuildID   string   `json:"build_id"`
	Directory string   `json:"directory"`
	Command   []string `json:"command"`
	Log       string   `json:"log"`
	// PackerLog is where Packer logs, and the only place QEMU's own messages
	// appear.
	PackerLog string `json:"packer_log"`
}

func (t *tools) start(_ context.Context, _ *mcp.CallToolRequest, input StartInput) (*mcp.CallToolResult, StartOutput, error) {
	build, err := t.builds.Start(builds.Request{
		Template:   input.Template,
		Vars:       input.Vars,
		VarFiles:   input.VarFiles,
		WorkingDir: input.WorkingDir,
		Env:        input.Env,
		ExtraArgs:  input.ExtraArgs,
		Debug:      input.Debug,
		MonitorVar: input.MonitorVar,
		Freeze:     input.Freeze,
		Settle:     time.Duration(input.SettleMs) * time.Millisecond,
	})
	if err != nil {
		return nil, StartOutput{}, err
	}

	output := StartOutput{
		BuildID:   build.Meta.ID,
		Directory: build.Dir.Path(),
		Command:   build.Meta.Command,
		Log:       build.Dir.LogPath(),
		PackerLog: build.Dir.PackerLogPath(),
	}
	return said("Started build %s: %s", output.BuildID, strings.Join(output.Command, " ")), output, nil
}

// ListOutput is the builds the host was found to have.
type ListOutput = builds.Listing

// ListInput says how much of the host's history to show.
type ListInput struct {
	All bool `json:"all,omitempty" jsonschema:"include builds that finished more than a day ago"`
}

func (t *tools) list(_ context.Context, _ *mcp.CallToolRequest, input ListInput) (*mcp.CallToolResult, *ListOutput, error) {
	found, err := t.builds.List(input.All)
	if err != nil {
		return nil, nil, err
	}
	return said("%s", listing(found)), found, nil
}

// StatusInput names a build to report on.
type StatusInput struct {
	BuildID  string `json:"build_id" jsonschema:"the build to report on"`
	LogLines int    `json:"log_lines,omitempty" jsonschema:"how many lines of the build's terminal to include"`
}

func (t *tools) status(_ context.Context, _ *mcp.CallToolRequest, input StatusInput) (*mcp.CallToolResult, *builds.Report, error) {
	report, err := t.builds.Status(input.BuildID, logLines(input.LogLines))
	if err != nil {
		return nil, nil, err
	}
	return said("%s", describe(report)), report, nil
}

// WaitInput names a build to wait for, and for how long.
type WaitInput struct {
	BuildID  string `json:"build_id" jsonschema:"the build to wait for"`
	Seconds  int    `json:"timeout_seconds,omitempty" jsonschema:"how long to wait; five minutes by default, an hour at most"`
	LogLines int    `json:"log_lines,omitempty" jsonschema:"how many lines of the build's terminal to include"`
}

func (t *tools) waitForPause(ctx context.Context, _ *mcp.CallToolRequest, input WaitInput) (*mcp.CallToolResult, *builds.Report, error) {
	report, err := t.builds.WaitForPause(ctx, input.BuildID, waitFor(input.Seconds), logLines(input.LogLines))
	if err != nil {
		return nil, nil, err
	}
	return said("%s", describe(report)), report, nil
}

// BuildInput names a build.
type BuildInput struct {
	BuildID string `json:"build_id" jsonschema:"the build to act on"`
}

// KeysInput is what to type, and at which build.
type KeysInput struct {
	BuildID string   `json:"build_id" jsonschema:"the build to type at"`
	Text    string   `json:"text,omitempty" jsonschema:"text to type character by character on a US keyboard"`
	Keys    []string `json:"keys,omitempty" jsonschema:"named keys and combinations to press after the text, such as down, ret or ctrl+alt+f2"`
	HoldMs  int      `json:"hold_ms,omitempty" jsonschema:"how long each key is held down; the emulator's own default if not given"`
}

func (t *tools) sendKeys(_ context.Context, _ *mcp.CallToolRequest, input KeysInput) (*mcp.CallToolResult, ActionOutput, error) {
	hold := time.Duration(input.HoldMs) * time.Millisecond
	if err := t.builds.SendKeys(input.BuildID, input.Text, input.Keys, hold); err != nil {
		return nil, ActionOutput{}, err
	}
	return said("Typed at build %s. Take a screenshot to see what it did.", input.BuildID),
		ActionOutput{BuildID: input.BuildID, Done: "typed"}, nil
}

// ClickInput is where to click, and at which build.
type ClickInput struct {
	BuildID string `json:"build_id" jsonschema:"the build to click at"`
	X       int    `json:"x" jsonschema:"how many pixels from the left of the screen"`
	Y       int    `json:"y" jsonschema:"how many pixels from the top of the screen"`
	Button  string `json:"button,omitempty" jsonschema:"which button: left, middle or right, left by default"`
	Times   int    `json:"times,omitempty" jsonschema:"how many times to press it, for a double click; once by default"`
}

func (t *tools) click(_ context.Context, _ *mcp.CallToolRequest, input ClickInput) (*mcp.CallToolResult, *builds.Clicked, error) {
	clicked, err := t.builds.Click(input.BuildID, image.Pt(input.X, input.Y),
		button(input.Button), times(input.Times))
	if err != nil {
		return nil, nil, err
	}
	return said("Clicked at (%d, %d) of a %dx%d screen. The screen as it was just before is %s; "+
			"take another screenshot to see what the click did.",
			clicked.X, clicked.Y, clicked.Screen.Width, clicked.Screen.Height, clicked.Screen.Path),
		clicked, nil
}

// ScreenshotOutput is where the capture was written.
type ScreenshotOutput struct {
	BuildID string `json:"build_id"`
	Path    string `json:"path"`
}

func (t *tools) screenshot(_ context.Context, _ *mcp.CallToolRequest, input BuildInput) (*mcp.CallToolResult, ScreenshotOutput, error) {
	path, err := t.builds.Screenshot(input.BuildID)
	if err != nil {
		return nil, ScreenshotOutput{}, err
	}
	return said("Wrote the guest's screen to %s; read it as an image. "+
			"Each capture is a new file; nothing is replaced.", path),
		ScreenshotOutput{BuildID: input.BuildID, Path: path}, nil
}

// ActionOutput is what became of an instruction.
type ActionOutput struct {
	BuildID string `json:"build_id"`
	Done    string `json:"done"`
}

// ContinueInput names a build to let carry on, and how far.
type ContinueInput struct {
	BuildID    string `json:"build_id" jsonschema:"the build to act on"`
	UntilLabel string `json:"until_label,omitempty" jsonschema:"run forward to the boot step whose label contains this, answering the pauses before it"`
}

func (t *tools) resume(_ context.Context, _ *mcp.CallToolRequest, input ContinueInput) (*mcp.CallToolResult, ActionOutput, error) {
	if err := t.builds.Continue(input.BuildID, input.UntilLabel); err != nil {
		return nil, ActionOutput{}, err
	}
	if input.UntilLabel != "" {
		return said("Build %s is running forward to %q; poll status to see it arrive.",
				input.BuildID, input.UntilLabel),
			ActionOutput{BuildID: input.BuildID, Done: "running to " + input.UntilLabel}, nil
	}
	return said("Build %s is running again.", input.BuildID),
		ActionOutput{BuildID: input.BuildID, Done: "continued"}, nil
}

// AbortInput names a build to stop.
type AbortInput struct {
	BuildID  string `json:"build_id" jsonschema:"the build to stop"`
	LogLines int    `json:"log_lines,omitempty" jsonschema:"how many lines of the build's terminal to include in the report"`
}

func (t *tools) abort(ctx context.Context, _ *mcp.CallToolRequest, input AbortInput) (*mcp.CallToolResult, *builds.Report, error) {
	report, err := t.builds.Abort(ctx, input.BuildID, logLines(input.LogLines))
	if err != nil {
		return nil, nil, err
	}
	if !report.Finished {
		return said("Build %s was told to stop and has not finished yet. Its output directory may "+
				"still be being deleted; wait before building into it again.\n%s", input.BuildID, describe(report)),
			report, nil
	}
	return said("Build %s has stopped.\n%s", input.BuildID, describe(report)), report, nil
}

func logLines(asked int) int {
	if asked <= 0 {
		return defaultLogLines
	}
	return asked
}

func waitFor(seconds int) time.Duration {
	asked := time.Duration(seconds) * time.Second
	switch {
	case asked <= 0:
		return defaultWait
	case asked > maximumWait:
		return maximumWait
	default:
		return asked
	}
}

func button(asked string) string {
	if asked == "" {
		return defaultButton
	}
	return asked
}

func times(asked int) int {
	if asked <= 0 {
		return 1
	}
	return asked
}

func said(format string, arguments ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, arguments...)}},
	}
}

func listing(found *builds.Listing) string {
	lines := make([]string, 0, len(found.Builds)+1)
	for _, summary := range found.Builds {
		lines = append(lines, fmt.Sprintf("%s  %s  %s  %s",
			summary.ID, summary.Elapsed, condition(summary), summary.Template))
	}
	if len(lines) == 0 {
		lines = append(lines, "No builds from the last day on this host.")
	}
	if found.Omitted > 0 {
		lines = append(lines, fmt.Sprintf("(%d older %s left out; pass all to see them.)",
			found.Omitted, plural(found.Omitted, "build")))
	}
	return strings.Join(lines, "\n")
}

func plural(count int, noun string) string {
	if count == 1 {
		return noun
	}
	return noun + "s"
}

func describe(report *builds.Report) string {
	lines := []string{
		fmt.Sprintf("Build %s, %s in: %s", report.ID, report.Elapsed, condition(report.Summary)),
		fmt.Sprintf("Last pause: %s", or(report.Label, "none yet")),
		fmt.Sprintf("Guest: %s", report.Guest),
		fmt.Sprintf("Packer's log, where QEMU's messages go: %s", report.PackerLog),
	}
	if report.Screenshot != "" {
		lines = append(lines, fmt.Sprintf("Capture at this pause (%s): %s",
			or(report.ScreenshotLabel, "unnamed step"), report.Screenshot))
	}
	if len(report.Trail) > 0 {
		lines = append(lines, fmt.Sprintf("Passed %d boot %s on the way here:",
			len(report.Trail), plural(len(report.Trail), "step")))
		for _, frame := range report.Trail {
			lines = append(lines, fmt.Sprintf("  %s  %s",
				frame.Label, or(frame.Screenshot, "(could not be photographed)")))
		}
	}
	if report.Note != "" {
		lines = append(lines, fmt.Sprintf("Note: %s", report.Note))
	}
	if len(report.Log) > 0 {
		lines = append(lines, "", strings.Join(report.Log, "\n"))
	}
	return strings.Join(lines, "\n")
}

func condition(summary builds.Summary) string {
	switch {
	case summary.Finished:
		return fmt.Sprintf("finished with exit status %d", summary.ExitCode)
	case summary.Awaiting != "":
		return fmt.Sprintf("running forward to %q", summary.Awaiting)
	case summary.Paused && summary.Frozen:
		return "waiting at a pause, guest frozen"
	case summary.Paused:
		return "waiting at a pause"
	default:
		return "running"
	}
}

func or(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
