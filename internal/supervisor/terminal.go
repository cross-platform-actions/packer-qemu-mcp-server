package supervisor

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// Terminal is the pseudo-terminal a build runs on, with the supervisor
// holding the other end of it.
//
// Packer's `-debug` pause is a read from the controlling terminal, not from
// standard input. A build with no terminal does not block on the prompt at
// all: the read fails, the pause silently does not happen, and the build runs
// on. Handing the build a terminal of its own is therefore the only way to
// step through it from a process that is not a terminal itself.
type Terminal struct {
	master  *os.File
	command *exec.Cmd
	done    chan struct{}
	reaped  sync.Once
}

// failedToStart stands in for the exit status of a build that never ran.
const failedToStart = -1

// answer is what the pause prompt is waiting for. Enter is a carriage return,
// and the prompt is read in raw mode, where nothing translates it to a
// newline for us.
const answer = "\r"

// Open starts the command on a new pseudo-terminal, in a session of its own so
// that the terminal is the command's controlling terminal and the command is
// the foreground process group on it. Packer checks for both before it will
// prompt.
func Open(command *exec.Cmd) (*Terminal, error) {
	master, slave, err := pty.Open()
	if err != nil {
		return nil, fmt.Errorf("opening a terminal for the build: %w", err)
	}
	defer slave.Close()

	command.Stdin = slave
	command.Stdout = slave
	command.Stderr = slave
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}

	if err := command.Start(); err != nil {
		master.Close()
		return nil, fmt.Errorf("starting the build: %w", err)
	}
	return &Terminal{master: master, command: command, done: make(chan struct{})}, nil
}

// Read returns what the build has written to its terminal.
func (t *Terminal) Read(buffer []byte) (int, error) {
	return t.master.Read(buffer)
}

// Answer presses enter at a pause prompt.
func (t *Terminal) Answer() error {
	if _, err := t.master.WriteString(answer); err != nil {
		return fmt.Errorf("answering the pause prompt: %w", err)
	}
	return nil
}

// PID is the build's process, which is also its process group.
func (t *Terminal) PID() int {
	return t.command.Process.Pid
}

// Terminate interrupts the build and everything it started, giving Packer the
// grace period it needs to shut the virtual machine down and clean up after
// itself before it is killed outright.
func (t *Terminal) Terminate(grace time.Duration) error {
	if err := t.signal(syscall.SIGINT); err != nil {
		return err
	}

	go func() {
		select {
		case <-t.done:
		case <-time.After(grace):
			// By now the build may well have gone on its own.
			_ = t.signal(syscall.SIGKILL)
		}
	}()
	return nil
}

// signal reaches the whole process group: Packer runs plugins and QEMU as
// separate processes, and interrupting Packer alone can leave them behind.
//
// A build that has already been waited for is left alone. Its number belongs
// to the system again the moment it is reaped, and the next process to be
// given that number is somebody else's — signalling it would be at best an
// error and at worst an unrelated process killed.
func (t *Terminal) signal(sig syscall.Signal) error {
	select {
	case <-t.done:
		return nil
	default:
	}

	if err := syscall.Kill(-t.PID(), sig); err != nil && err != syscall.ESRCH {
		return fmt.Errorf("signalling the build: %w", err)
	}
	return nil
}

// Wait returns the build's exit status once it has finished. It may be called
// from more than one place — a supervisor that gives up early waits for the
// build itself — so it settles who reports the exit only once.
func (t *Terminal) Wait() int {
	t.reaped.Do(func() {
		// The error only restates the exit status, which is read back below.
		_ = t.command.Wait()
		close(t.done)
	})
	<-t.done
	if t.command.ProcessState == nil {
		return failedToStart
	}
	return t.command.ProcessState.ExitCode()
}

// Close releases the terminal.
func (t *Terminal) Close() error {
	return t.master.Close()
}
