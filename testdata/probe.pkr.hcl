# A build small enough to iterate on: no provisioners, no communicator, and a
# disc image that deliberately does not boot. It reaches the boot steps in
# about ten seconds, which is where the interesting pauses are, and then ends.
#
# Do not debug boot synchronisation with a two-hour build.

packer {
  required_plugins {
    qemu = {
      source  = "github.com/hashicorp/qemu"
      version = ">= 1.1.1"
    }
  }
}

# Supplied by the MCP server, one socket per build. Packer holds the monitor it
# opens itself, and QEMU serves one monitor connection at a time, so the
# harness needs a second one of its own.
variable "qmp_socket" {
  type        = string
  description = "path for the harness's own QEMU monitor socket"
}

variable "iso_url" {
  type        = string
  default     = "./blank.iso"
  description = "point this at a real image to watch a real installation"
}

source "qemu" "probe" {
  iso_url      = var.iso_url
  iso_checksum = "none"

  disk_size    = "64M"
  memory       = 128
  headless     = true
  communicator = "none"

  # Each build writes into its own state directory, so two runs never collide
  # over an output directory that already exists.
  output_directory = "${dirname(var.qmp_socket)}/out"

  boot_wait = "2s"
  # Labelled like an installer's, so that continue(until_label:) has something
  # to aim at.
  boot_steps = [
    ["<enter>", "first keystroke"],
    ["<enter>", "second keystroke"],
    ["<enter>", "third keystroke"],
  ]

  # -qmp is not a flag Packer sets itself, so this appends rather than
  # replaces. Naming a flag Packer does set would override Packer's version.
  qemuargs = [
    ["-qmp", "unix:${var.qmp_socket},server=on,wait=off"],
  ]
}

build {
  sources = ["source.qemu.probe"]
}
