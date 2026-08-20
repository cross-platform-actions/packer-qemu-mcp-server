<p align="center">
  <img src="logo.svg" alt="packer-qemu-mcp-server logo" width="400">
</p>

# packer-qemu-mcp-server

An MCP server that lets an agent see and steer an interactive operating system
installation driven by Packer's QEMU builder, instead of waiting blind for an
SSH connection that may never arrive.

Between the boot command and *Waiting for SSH to become available*, Packer has
no feedback channel: it types keystrokes at a framebuffer it cannot read. When
the guest is not on the screen Packer assumes — an ISO respun, a boot menu
shifted, a `<wait>` too short — the rest of the boot command lands in the wrong
place and the build waits out the whole SSH timeout for nothing. A serial
console does not settle it either: if `console=ttyS0` was typed into the wrong
screen, silence cannot tell "didn't boot" from "booted but the kernel arguments
never applied". A picture of the framebuffer can.

## Build and install

```
go build -o packer-qemu-mcp-server ./cmd/packer-qemu-mcp-server
```

Prebuilt binaries for Linux and macOS, amd64 and arm64, are on the
[releases page](https://github.com/cross-platform-actions/packer-qemu-mcp-server/releases).

Unix only, by design: it allocates a pseudo-terminal for the build and reaches
the running build over unix domain sockets. There is no Windows build.

Register it with an MCP client as a stdio server, for example with Claude Code:

```
claude mcp add packer-qemu-mcp-server -- /path/to/packer-qemu-mcp-server
```

**Rebuilding it is not enough.** A client settles which tools a server offers
when it connects, so a session that was already running will not find a tool
that has since been added, however new the binary behind it is. The tool looks
missing, which reads as a build that silently did not take. Reconnect — `/mcp`
in Claude Code — or start a fresh session; builds are unaffected, since they
are detached and rediscovered from disk, so it costs nothing.

Whether a *changed* description reaches a session already running is up to the
client, and at least one fetches them live. So a description that is visibly up
to date is not evidence that a newly added tool would be found too.

Two optional environment variables:

| Variable | Meaning | Default |
|---|---|---|
| `PK_BUILDS_DIR` | where builds keep their state | `/tmp/pk-builds` |
| `PACKER_BIN` | the packer binary to run | `packer` |

## What the template has to do

Packer can open a monitor socket itself, but it holds that connection, and QEMU
serves one monitor connection at a time. So the template opens a second,
independent socket, whose path the harness passes in:

```hcl
variable "qmp_socket" { type = string }

source "qemu" "example" {
  # …
  qemuargs = [
    ["-qmp", "unix:${var.qmp_socket},server=on,wait=off"],
  ]
}
```

`-qmp` is not a flag Packer sets itself, so this appends rather than replaces.
Without it the build still runs and can still be stepped through, but there are
no screenshots and no freezing.

Keep `PK_BUILDS_DIR` short: the monitor socket lives inside the build's
directory, and a unix socket path may not exceed about a hundred bytes.

## The tools

| Tool | What it does |
|---|---|
| `start_build(template, …)` | starts a detached build and returns its id |
| `list_builds(all)` | the builds on the host, rediscovered from disk |
| `status(build_id)` | last pause, freeze, guest state, last lines of the log |
| `wait_for_pause(build_id, timeout_seconds)` | blocks until the build reaches a pause, or finishes |
| `screenshot(build_id)` | writes a PNG of the guest's screen and returns the path |
| `send_keys(build_id, text, keys)` | types at the guest through the emulated keyboard |
| `click(build_id, x, y)` | presses a mouse button at a point on the guest's screen |
| `continue(build_id, until_label)` | resumes the guest, then lets the build carry on — as far as a named boot step, if you give one |
| `abort(build_id)` | resumes the guest, stops the build, and waits for it to be gone |

`status` reports the *last pause*, not the step the build is running: under
`-debug` a step is announced as it finishes, so the build is always somewhere
past it. The log is the live view.

### Asking the guest a question

`send_keys` and `click` reach the guest through the emulator, so they need
nothing running inside it and work at a boot loader or an installer menu, where
nothing can be asked anything. That turns "why did the disk not appear?" from a
throwaway template and a six-minute rebuild into opening a terminal and running
`listdev`:

```
send_keys(build_id, text: "listdev\n")
screenshot(build_id)
```

`text` is typed character by character on a US keyboard; `keys` are named keys
and combinations sent after it — `down`, `ret`, `ctrl+alt+f2`, `f12`. The guest
has to be running, so `continue` first: a frozen guest reads nothing, and
keystrokes sitting in the emulated keyboard look exactly like keystrokes that
went astray.

`click` takes pixels of the screenshot the point was read off, and photographs
the screen first — both to measure it and to leave a record of what was clicked
on. It needs a template with an absolute pointing device, since the default
PS/2 mouse reports movements rather than positions:

```hcl
qemuargs = [
  ["-qmp", "unix:${var.qmp_socket},server=on,wait=off"],
  ["-device", "qemu-xhci"],
  ["-device", "usb-tablet"],
]
```

The controller matters: neither `pc` nor `q35` gives a machine a USB bus of its
own, and `usb-tablet` alone fails to start with `No 'usb-bus' bus found`. Naming
`-device` in `qemuargs` replaces the plugin's own, but it puts the network
device back if it is missing, so this does not cost you networking.

### When a build will not start

QEMU's own messages go to Packer's log and nowhere else: the plugin captures
the emulator's standard error and logs it, and none of it reaches the terminal.
`start_build` and `status` both report that file's path, and a build that dies
on the spot carries the tail of it back in the error. It is the difference
between *Qemu failed to start, please run with PACKER_LOG=1* and
`-display gtk: unsupported display`.

### Freezing is an invariant, not a tool

> While the build is paused at an interesting step, the guest is frozen.

The harness freezes the guest the moment it reaches such a pause, and thaws it
in `continue` and `abort`. There is deliberately no `stop` tool, for two
reasons. The freeze has to span the agent's *thinking* time — screenshot,
deliberate, continue — so it cannot be folded into `screenshot` as an atomic
stop-dump-continue. And a manual freeze would have a very long fuse: freezing
the guest does not pause Packer's timers, which are wall-clock, so a forgotten
freeze silently burns `ssh_timeout` and surfaces as an unexplained timeout
much later.

Which pauses are interesting follows from that:

| Pause | Frozen? | Screenshot? |
|---|---|---|
| Before the virtual machine is started (ISO, disk, HTTP server) | Nothing to freeze | No |
| Each `boot_steps` entry | Yes, automatically | Yes, every one: a desync can start at any entry |
| A `boot_steps` entry run past with `until_label` | No: the build is on its way somewhere | Yes, into the trail |
| After the last boot step | Yes | Yes — the highest-value single picture: did the whole sequence land? |
| Inside `StepConnect`, waiting for SSH | No: `ssh_timeout` is ticking and freezing does not pause it | On demand |
| Between provisioners | No: an SSH session is established, and a long freeze risks killing it | Rarely useful |

Every other pause — downloading the ISO, creating the disk, each cleanup step —
is answered immediately, without asking.

**`screenshot` works at any time, not only at a pause.** That is the main mode:
under emulation the installation happens inside a single Packer step, waiting
for SSH, where `-debug` provides no pauses at all.

### What the picture shows

The capture at a boot step is the screen **as that step finished**, and usually
it already shows what the step's keystrokes did. Packer leaves one key interval
after the last keystroke of a step before it announces the pause — 100ms by
default, `PACKER_KEY_INTERVAL` or `boot_key_interval` to change it — and a
guest that is keeping up has redrawn in that time. Measured on a Haiku
installer: at the pause after *Press 'Set up partitions'*, DriveSetup was
already open; at the pause after the first `<down>`, the row was already
highlighted.

On a slow or busy guest it may instead be the screen those keystrokes landed
on, which is why it is worth reading as "the end of this step" rather than
assuming either. `start_build(settle_ms: 500)` widens the gap if a guest is
consistently caught mid-redraw. A second is the ceiling: one goroutine per
build answers both its pauses and its instructions, so a build settling is a
build not listening, and a longer settle would have it refuse to be stopped.

What the guest does *not* do is drift on its own. Measured on an otherwise idle
guest: three captures 72 seconds apart were byte-identical, while letting it
run for one second changed the screen. So a frozen guest stays exactly as
photographed for as long as an agent takes to think.

This section used to say the opposite — that the capture was the screen the
keystrokes landed on, taken before the guest could react. It was wrong, and it
was wrong in the direction that costs the most: an agent told to expect a stale
screen dismisses good evidence.

### When a keystroke goes missing

It happens, and it is not this. A guest occasionally drops the first character
sent to a widget that has just taken focus — measured on a Haiku installer
under TCG on Apple silicon, at two unrelated places: a `<down>` into a list
that had just opened, costing one row, and the `a` of `apps<enter>` into
Tracker's type-ahead, which then matched on `p` and opened the wrong folder
entirely. Roughly one run in three, never on demand.

`-debug` was the first suspect and is not the culprit. A build with
`debug: false` — no pauses, no freezing anywhere — lost the same keystroke in
the same way. The freeze had already been ruled out on the code: a step passed
while running forward to a named one is answered without stopping the guest at
all, so during those steps there is nothing to blame.

The practical answer is to write the step so that losing its first keystroke
does not change where it lands. Counting keypresses is what makes a boot
command brittle:

```hcl
// Fragile: three presses from a list that opens unselected, where the first
// press lands on row 1 or row 2 depending on focus.
["<down><wait>", "…"], ["<down><wait>", "…"], ["<down><wait>", "…"],

// Insensitive: the selection clamps at the bottom rather than wrapping, so
// pressing past the end arrives from anywhere — and absorbs a lost press.
["<down><down><down><down><down><down><wait>", "…"],
```

When it does happen, the trail from `until_label` is what says which step it
happened at. `start_build` also takes `freeze: false`, which runs the build
with its pauses but never stops the guest, and `settle_ms`, which stops it a
moment later; both exist to rule this harness out of a keystroke problem, and
so far they always have.

### Stepping through sixty of them

A real installer's boot command runs to dozens of entries — the NetBSD builder
that motivated this has around sixty — and every one of them pauses. Naming a
step runs the build forward to it:

```
continue(build_id, until_label: "Compiler tools")
```

Pauses before it are answered as they arrive, without freezing — the guest is
on its way somewhere — but each one is photographed, and `status` reports the
trail:

```
Passed 12 boot steps on the way here:
  DVD 1 - Haiku                              …/shots/20260822-163859.101-a1.png
  DVD 1 - haiku eps                          …/shots/20260822-163900.204-b2.png
  /dev/disk/scsi/1/0/0/raw                   …/shots/20260822-163901.377-c3.png
```

That trail is the point. A desync begins at one step and is noticed at another,
and a picture only of where the build ended up says nothing about which of the
fifty skipped steps went wrong. The trail starts again at each `continue`, so
it always describes the leg just travelled.

The named step stops and freezes like any other. Matching is on any part of the
label, ignoring case. A label that never matches stops the build at the end of
the boot command rather than letting it run on into the wait for SSH unwatched,
and says so. The call returns as soon as the build is moving — poll `status`,
or `wait_for_pause`.

## How it is put together

A build takes hours under emulation, far longer than anything that watches it,
so nothing that watches it may own it:

```
MCP server  ─ starts ─→  supervisor  ─ holds the terminal of ─→  packer build
 (stateless)             (one per build, detached)                    │
     └──── reads ────→  /tmp/pk-builds/<id>/  ←──── publishes ────────┘
                        meta.json  state.json  log  ctl.sock  mon.sock  shots/
```

Restart the client, restart the server, and the build keeps going; the server
finds it again by reading the directory.

### Why a pseudo-terminal, and not a FIFO

Packer's `-debug` pause is a read from the *controlling terminal*, not from
standard input. A build detached with its stdin on a named pipe does not pause
at all: the read fails with `no available tty`, and Packer runs straight through
every step — the worst of both worlds, since `-debug` then silently does
nothing. The supervisor therefore gives the build a pseudo-terminal of its own,
with the build as the session leader and the foreground process group, which is
what Packer checks before it will prompt. Answering the prompt means writing a
carriage return: the prompt is read in raw mode, where a newline is not what
Enter sends.

If Packer ever grows a way to answer the pause off the terminal — a
`-debug-socket`, say — the supervisor is the only part of this that would go
away.

## Trying it by hand

No agent required. [`testdata/`](testdata) holds a build small enough to
iterate on, and the MCP Inspector gives every tool a form:

```
go build -o packer-qemu-mcp-server ./cmd/packer-qemu-mcp-server
npx @modelcontextprotocol/inspector ./packer-qemu-mcp-server
```

Fill in `start_build` with nothing but the path to `testdata/probe.pkr.hcl`,
then call `status`, `screenshot`, `continue` and `abort` with the build id it
returns. The build stops at its first boot step with the guest frozen, and the
screenshot is of firmware failing to boot a stub image — a real screen.

`start_build` echoes back the command it ran, which is the quickest way to
check that `-debug` is really on: a client that sends `debug: false` rather
than omitting it gets a build with no pauses at all, which looks like the
harness ignoring them.

Without a client at all, the protocol is three lines of JSON. Keep stdin open
afterwards, or the server shuts down before it answers:

```bash
{ printf '%s\n' \
    '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"by-hand","version":"0"}}}' \
    '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
    '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_builds","arguments":{}}}'
  sleep 2
} | ./packer-qemu-mcp-server
```

## Development

```
go test -race ./...
golangci-lint run ./...
gofmt -l .
```

The tests need no Packer and no QEMU: a fake QEMU monitor stands in for the
emulator, and a shell script that prints Packer's pause prompts stands in for a
build. The pseudo-terminal, the sockets and the detaching are real, which is
why CI runs them on every platform released rather than only where a runner
exists:

| Where | How |
|---|---|
| Linux and macOS on x86-64 | GitHub's own runners |
| Linux on 386, arm, arm64 and ppc64le | a container of that architecture under QEMU, as [cross-platform-actions recommends][cpa-linux] |
| FreeBSD, NetBSD, OpenBSD, Solaris and illumos | a virtual machine, through [cross-platform-actions][cpa] |

In both of the emulated cases the test binaries are cross-compiled on the
runner and only executed over there, so neither the container nor the guest
needs a Go toolchain. The Linux ones are static, so the container needs nothing
but a shell for the fake builds the tests write; on Solaris a Go binary links
libc, libsocket and libsendfile, which the guest has.

The last row is one OmniOS guest, which is illumos rather than Oracle
Solaris — the closest a CI job can get, since nobody offers Oracle Solaris
runners. illumos is a fork of OpenSolaris and keeps its userland ABI: the
binaries built for either target ask the same loader for the same three
libraries. The tests are built for `solaris`, which is what the release ships,
and for `illumos` as well, so that a failure says which of the two is at fault.

[cpa]: https://github.com/cross-platform-actions/action
[cpa-linux]: https://github.com/cross-platform-actions/action/#linux-on-non-x86-architectures

Every change gets an entry under `[Unreleased]` in
[changelog.md](changelog.md), under the sub header it belongs to (`Added`,
`Changed`, `Deprecated`, `Removed`, `Fixed`, `Security`). Those sub headers are
what decides the next version.

Releases are cut with [relog], from a clean `master`. It reads the sub headers
under `[Unreleased]` to pick the bump — `### Fixed` alone is a patch, `### Added`,
`### Changed` or `### Deprecated` a minor, `### Removed` or the word "Breaking"
anywhere a major — rewrites that section as `## [X.Y.Z]` with today's date,
commits it, creates an annotated `vX.Y.Z` tag and asks before pushing.
`relog --dry-run` shows what it would do without doing it, and `relog X.Y.Z`
overrides the version it picked. There is no `release.conf`: the defaults find
`changelog.md` and `master`, and nothing in this repo names the current version
outside the changelog, so there is nothing for a hook to rewrite.

[relog]: https://github.com/jacob-carlborg/relog

Pushing the tag is what releases. GoReleaser builds the binaries once CI is
green and creates a *draft* release called `packer-qemu-mcp-server X.Y.Z`, whose
notes are the changelog entry relog has just written, read back out of the file
by the release workflow. Read the draft and publish it by hand.

The binaries are one for every platform the [QEMU plugin][plugin] publishes for,
except Windows, which has neither a pseudo-terminal nor a unix socket to offer.
The plugin rather than Packer itself, since a build this can watch is a build
the plugin can run; the two lists are identical in any case.

illumos gets a binary too, which the plugin does not publish. A Solaris binary
runs on illumos — CI demonstrates it on an OmniOS guest on every run — so this
is a convenience rather than a necessity, and Solaris keeps its own binary
because nothing can be tested on Oracle Solaris and dropping it would leave
those users with nothing.

[plugin]: https://github.com/hashicorp/packer-plugin-qemu

The Linux binaries need nothing at runtime but the kernel: no libc, no dynamic
loader, nothing to resolve. They run on any distribution, and in a container
with nothing else in it. CI checks that on every build rather than trusting it.

## License

MIT. See [license](license).
