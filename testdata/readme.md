# A build to try the server on

`probe.pkr.hcl` is the cut-down build worth iterating on: a small disk, no
communicator, no provisioners, and a disc image that deliberately does not
boot. It reaches the boot steps in about ten seconds and then ends, so the
cycle is short enough to actually learn something from.

The one field it needs is its own path. Everything else — the monitor socket,
`-debug`, the output directory — is arranged for you.

```
start_build   template = <this directory>/probe.pkr.hcl
status        build_id = <what start_build returned>
screenshot    build_id = <the same>
continue      build_id = <the same>
abort         build_id = <the same>
```

`status` will show the build waiting at `first keystroke` with the guest
frozen, and `screenshot` writes a PNG of the firmware failing to boot the
stub image — a real screen, drawn by real firmware, which is the whole point.
`continue` moves it to `second keystroke`.

`blank.iso` is 2048 bytes of text. QEMU tries to boot it, says why it cannot,
and drops to iPXE. Point `iso_url` at a real image when you want to watch a
real installation:

```
start_build   template = …/probe.pkr.hcl
              vars     = {"iso_url": "/path/to/something-real.iso"}
```

Nothing here runs in CI: it needs Packer, QEMU and an installed qemu plugin.
It is here so that trying the server out does not start with building a
template first.
