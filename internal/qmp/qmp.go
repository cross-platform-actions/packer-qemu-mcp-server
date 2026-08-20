// Package qmp talks to a QEMU monitor socket.
//
// The monitor controls the emulator, not the guest: it needs no agent inside
// the virtual machine and the guest cannot tell it is there. That is what
// makes it usable during an installation that has not yet reached a state
// where anything can be asked of the guest.
package qmp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// ErrUnavailable is returned when the monitor socket cannot be reached. Before
// Packer starts the virtual machine there is no QEMU and no socket, and
// callers are expected to carry on without a screenshot rather than fail.
var ErrUnavailable = errors.New("qemu monitor is not available")

// ErrBusy is returned when another process held the monitor for so long that
// there was no time left to use it. The monitor is there; it is just taken.
var ErrBusy = errors.New("qemu monitor is busy with another session")

// DefaultTimeout bounds a whole monitor session, including waiting for another
// process to finish with the socket.
const DefaultTimeout = 5 * time.Second

// Client runs commands against one QEMU monitor socket.
//
// QEMU serves a single monitor connection at a time, so a client connects,
// runs one command and hangs up; a lock file next to the socket keeps
// concurrent clients — the supervisor freezing a guest, an agent asking for a
// screenshot — from queueing up behind each other unbounded.
type Client struct {
	Timeout time.Duration

	socket string
	lock   string
}

// New returns a client for the monitor socket at the given path.
func New(socket string) *Client {
	return &Client{
		Timeout: DefaultTimeout,
		socket:  socket,
		lock:    socket + ".lock",
	}
}

// Status is the emulator's view of the guest: whether its virtual CPUs are
// running, and why they are not.
type Status struct {
	Status  string `json:"status"`
	Running bool   `json:"running"`
}

// Status asks the emulator whether the guest is running, paused, or has
// panicked — states that a build waiting for SSH cannot tell apart.
func (c *Client) Status() (Status, error) {
	var status Status
	reply, err := c.run(command{Execute: "query-status"})
	if err != nil {
		return status, err
	}
	if err := json.Unmarshal(reply, &status); err != nil {
		return status, fmt.Errorf("reading guest status: %w", err)
	}
	return status, nil
}

// Screendump writes the emulated graphics card's framebuffer to a PNG file.
// The file is written by the QEMU process, so the path has to be writable by
// whoever runs the build.
func (c *Client) Screendump(path string) error {
	_, err := c.run(command{
		Execute:   "screendump",
		Arguments: map[string]any{"filename": path, "format": "png"},
	})
	return err
}

// Stop freezes the guest's virtual CPUs. It does not pause Packer, whose
// timers are wall-clock.
func (c *Client) Stop() error {
	_, err := c.run(command{Execute: "stop"})
	return err
}

// Cont resumes a frozen guest.
func (c *Client) Cont() error {
	_, err := c.run(command{Execute: "cont"})
	return err
}

// absMax is the value QEMU's absolute pointer axes run to, whatever the
// resolution of the screen behind them.
const absMax = 0x7FFF

// SendKeys presses the given keys together and releases them, which is how a
// chord such as ctrl-alt-f2 is sent as well as how a single key is. The names
// are QEMU's own key codes.
//
// This reaches the guest through the emulated keyboard, so it needs nothing
// running inside the guest and works at a boot prompt or an installer menu.
// It does nothing to a frozen guest but fill its keyboard buffer.
func (c *Client) SendKeys(codes []string, hold time.Duration) error {
	keys := make([]any, 0, len(codes))
	for _, code := range codes {
		keys = append(keys, map[string]any{"type": "qcode", "data": code})
	}

	arguments := map[string]any{"keys": keys}
	if hold > 0 {
		arguments["hold-time"] = int(hold.Milliseconds())
	}
	_, err := c.run(command{Execute: "send-key", Arguments: arguments})
	return err
}

// Click moves the pointer to a point on the screen and presses a button there.
// The coordinates are fractions of the screen, from 0 in the top left corner
// to 1 in the bottom right, because the emulated tablet reports a position
// rather than a movement and knows nothing of pixels.
//
// It needs an absolute pointing device — usb-tablet, or the virtio equivalent.
// A template with only the default PS/2 mouse has nothing to send this to, and
// QEMU says so.
func (c *Client) Click(x, y float64, button string, times int) error {
	events := []any{
		map[string]any{"type": "abs", "data": map[string]any{"axis": "x", "value": axis(x)}},
		map[string]any{"type": "abs", "data": map[string]any{"axis": "y", "value": axis(y)}},
	}
	for range times {
		events = append(events,
			map[string]any{"type": "btn", "data": map[string]any{"down": true, "button": button}},
			map[string]any{"type": "btn", "data": map[string]any{"down": false, "button": button}})
	}

	_, err := c.run(command{Execute: "input-send-event", Arguments: map[string]any{"events": events}})
	return err
}

// axis places a fraction of the screen on the axis QEMU reports positions on,
// clamped: a point outside the screen is a mistake worth not acting on, and
// the edge is the nearest thing to what was meant.
func axis(fraction float64) int {
	switch {
	case fraction <= 0:
		return 0
	case fraction >= 1:
		return absMax
	default:
		return int(fraction * absMax)
	}
}

type command struct {
	Execute   string         `json:"execute"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// run holds the socket for exactly one command: connect, negotiate, execute,
// hang up.
func (c *Client) run(cmd command) (json.RawMessage, error) {
	deadline := time.Now().Add(c.Timeout)

	release, err := c.acquire(deadline)
	if err != nil {
		return nil, err
	}
	defer release()

	// Waiting for the lock can use up the whole budget. Dialling with nothing
	// left would fail, and be reported as a monitor that is not there — the
	// opposite of the truth, which is that it is busy.
	if !time.Now().Before(deadline) {
		return nil, ErrBusy
	}

	conn, err := net.DialTimeout("unix", c.socket, time.Until(deadline))
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrUnavailable, c.socket, err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}

	session := &session{decoder: json.NewDecoder(conn), encoder: json.NewEncoder(conn)}
	if err := session.greet(); err != nil {
		return nil, err
	}
	return session.execute(cmd)
}

// gates serialises monitor sessions within this process, one gate per socket.
//
// The lock file alone would not: a POSIX record lock belongs to the process
// that took it, so two goroutines here would each be granted it and both talk
// to a monitor that serves one connection at a time. flock(2) would have had
// the right granularity, but it does not exist on Solaris, which Packer
// publishes for.
var gates sync.Map

func gate(path string) chan struct{} {
	empty, _ := gates.LoadOrStore(path, make(chan struct{}, 1))
	return empty.(chan struct{})
}

// wholeFile is the region to lock: from the start, for as long as the file is.
func wholeFile(kind int16) *unix.Flock_t {
	return &unix.Flock_t{Type: kind, Whence: io.SeekStart, Start: 0, Len: 0}
}

// acquire takes the socket's lock, whose only purpose is to serialise monitor
// sessions — between the goroutines of this process, and between this process
// and any other. Both halves are released when the returned function is
// called, and the file lock is released by the kernel if the holder dies.
func (c *Client) acquire(deadline time.Time) (func(), error) {
	entrance := gate(c.lock)
	select {
	case entrance <- struct{}{}:
	case <-time.After(time.Until(deadline)):
		return nil, ErrBusy
	}
	leave := func() { <-entrance }

	file, err := os.OpenFile(c.lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		leave()
		return nil, fmt.Errorf("opening monitor lock: %w", err)
	}

	for {
		if err := unix.FcntlFlock(file.Fd(), unix.F_SETLK, wholeFile(unix.F_WRLCK)); err == nil {
			return func() {
				// Closing the file would release the lock on its own; this
				// only makes the release the obvious thing that happens.
				_ = unix.FcntlFlock(file.Fd(), unix.F_SETLK, wholeFile(unix.F_UNLCK))
				file.Close()
				leave()
			}, nil
		}
		if time.Now().After(deadline) {
			file.Close()
			leave()
			return nil, ErrBusy
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// session is one connection to the monitor.
type session struct {
	decoder *json.Decoder
	encoder *json.Encoder
}

// greet reads QEMU's banner and negotiates capabilities. Every command before
// the handshake fails, so this is not optional.
func (s *session) greet() error {
	var banner struct {
		QMP *struct{} `json:"QMP"`
	}
	if err := s.decoder.Decode(&banner); err != nil {
		return greetingError(err)
	}
	if banner.QMP == nil {
		return fmt.Errorf("qemu monitor sent no greeting")
	}
	_, err := s.execute(command{Execute: "qmp_capabilities"})
	return err
}

func greetingError(err error) error {
	if errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: connection closed before the greeting", ErrUnavailable)
	}
	return fmt.Errorf("reading qemu greeting: %w", err)
}

// execute sends a command and waits for its reply. Replies interleave with
// asynchronous events, so read until something is a reply.
func (s *session) execute(cmd command) (json.RawMessage, error) {
	if err := s.encoder.Encode(cmd); err != nil {
		return nil, fmt.Errorf("sending %s: %w", cmd.Execute, err)
	}

	for {
		var message struct {
			Return json.RawMessage `json:"return"`
			Error  *struct {
				Class string `json:"class"`
				Desc  string `json:"desc"`
			} `json:"error"`
		}
		if err := s.decoder.Decode(&message); err != nil {
			return nil, fmt.Errorf("reading reply to %s: %w", cmd.Execute, err)
		}
		switch {
		case message.Error != nil:
			return nil, fmt.Errorf("%s failed: %s: %s", cmd.Execute, message.Error.Class, message.Error.Desc)
		case message.Return != nil:
			return message.Return, nil
		}
	}
}
