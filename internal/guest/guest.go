// Package guest is the virtual machine a build is installing into.
//
// Everything here goes through QEMU's monitor, which controls the emulator
// rather than the guest: it needs no agent inside the virtual machine and the
// guest cannot tell it is there. That is what makes it usable during an
// installation that has not reached a state where anything can be asked of the
// guest — a boot loader, an installer, a kernel still coming up.
//
// Both sides of the harness talk to a guest. The supervisor freezes one and
// photographs it at a pause; the build service photographs one on demand and
// types at it. They are the same virtual machine and the same conversation, so
// they are the same object.
package guest

import (
	"fmt"
	"image"
	_ "image/png" // screenshots are PNGs, and their size is read from the header
	"os"
	"time"

	"github.com/cross-platform-actions/packer-qemu-mcp-server/internal/keyboard"
	"github.com/cross-platform-actions/packer-qemu-mcp-server/internal/qmp"
)

// Guest is one virtual machine, and the one rule the harness keeps about it:
// while the build is paused somewhere interesting, the guest is frozen.
//
// The rule cannot be left to whoever is looking at the build. `-debug` pauses
// Packer, not the guest, so a boot menu counts down and a boot command lands
// in the wrong place while an agent thinks about the screenshot it just took —
// manufacturing exactly the desync the screenshot was taken to diagnose.
type Guest struct {
	monitor *qmp.Client
	frozen  bool
}

// At returns the guest reachable through the given monitor socket. The socket
// does not have to exist yet: before Packer starts the virtual machine there
// is no QEMU behind it.
func At(socket string) *Guest {
	return &Guest{monitor: qmp.New(socket)}
}

// Freeze stops the guest's virtual CPUs.
func (g *Guest) Freeze() error {
	if err := g.monitor.Stop(); err != nil {
		return err
	}
	g.frozen = true
	return nil
}

// Resume starts a frozen guest again, and does nothing to a running one.
func (g *Guest) Resume() error {
	if !g.frozen {
		return nil
	}
	if err := g.monitor.Cont(); err != nil {
		return err
	}
	g.frozen = false
	return nil
}

// Frozen reports whether the harness is holding the guest still.
func (g *Guest) Frozen() bool { return g.frozen }

// Condition is what the emulator says about the guest: whether its virtual
// CPUs are running, and what it calls the state they are in. A build waiting
// for SSH cannot tell those apart.
type Condition struct {
	Name    string
	Running bool
}

// Condition asks the emulator how the guest is.
func (g *Guest) Condition() (Condition, error) {
	status, err := g.monitor.Status()
	if err != nil {
		return Condition{}, err
	}
	return Condition{Name: status.Status, Running: status.Running}, nil
}

// Capture writes the guest's screen to a PNG file. The file is written by the
// QEMU process, so the path has to be writable by whoever runs the build.
func (g *Guest) Capture(path string) error {
	return g.monitor.Screendump(path)
}

// A Screen is a capture of the guest's framebuffer, and how big it turned out
// to be. The size is what turns a point read off the picture into a point the
// emulated tablet understands.
type Screen struct {
	Path   string `json:"path"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// Holds reports whether a point is on the screen at all.
func (s Screen) Holds(at image.Point) bool {
	return at.X >= 0 && at.Y >= 0 && at.X < s.Width && at.Y < s.Height
}

// across is how far along each axis a point is, which is how the emulated
// tablet reports a position: a fraction of the screen, knowing nothing of
// pixels.
func (s Screen) across(at image.Point) (float64, float64) {
	return float64(at.X) / float64(s.Width), float64(at.Y) / float64(s.Height)
}

// Photograph captures the guest's screen and measures it.
func (g *Guest) Photograph(path string) (Screen, error) {
	if err := g.Capture(path); err != nil {
		return Screen{}, err
	}
	return measure(path)
}

// measure reads a capture's dimensions from its header, without decoding the
// picture itself.
func measure(path string) (Screen, error) {
	file, err := os.Open(path)
	if err != nil {
		return Screen{}, fmt.Errorf("reading the capture just taken: %w", err)
	}
	defer file.Close()

	config, _, err := image.DecodeConfig(file)
	if err != nil {
		return Screen{}, fmt.Errorf("reading the size of %s: %w", path, err)
	}
	return Screen{Path: path, Width: config.Width, Height: config.Height}, nil
}

// Type presses the given keys at the guest, one press at a time, holding each
// for the given time and leaving the given gap between them. A guest emulated
// on a slow host needs the gap; one press must also not be sent while the
// previous one is still down, so the hold sets the gap if it is longer.
func (g *Guest) Type(presses []keyboard.Press, hold, gap time.Duration) error {
	for at, press := range presses {
		if at > 0 {
			time.Sleep(max(gap, hold))
		}
		if err := g.monitor.SendKeys(press, hold); err != nil {
			return fmt.Errorf("after %d of %d key presses: %w", at, len(presses), err)
		}
	}
	return nil
}

// Point presses a mouse button at a point on the given screen.
//
// It needs an absolute pointing device — usb-tablet, or the virtio
// equivalent — because the position is what is being sent. A template with
// only the default PS/2 mouse, which reports movements, has nothing to send
// this to, and QEMU says so.
func (g *Guest) Point(screen Screen, at image.Point, button string, times int) error {
	across, down := screen.across(at)
	return g.monitor.Click(across, down, button, times)
}
