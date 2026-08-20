package builds

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/cross-platform-actions/packer-qemu-mcp-server/internal/buildstate"
)

// SuperviseCommand is the subcommand that turns this program into one build's
// supervisor.
const SuperviseCommand = "supervise"

// DetachedLauncher starts a build's supervisor as a session of its own.
//
// A build under emulation takes hours, and nothing that watches it lives that
// long: an agent's shell calls are capped at minutes, and the MCP server
// itself can be reloaded or crash. So the supervisor is not a child anyone
// waits on — it is detached, and the build it holds survives everything above
// it.
type DetachedLauncher struct {
	// Executable is the program to re-run as the supervisor. It defaults to
	// this one.
	Executable string
}

// Launch starts the supervisor for the given build and returns without waiting
// for it.
func (l *DetachedLauncher) Launch(dir *buildstate.Dir) error {
	executable, err := l.executable()
	if err != nil {
		return err
	}

	output, err := os.OpenFile(dir.SupervisorLogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("opening the supervisor log: %w", err)
	}
	defer output.Close()

	empty, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer empty.Close()

	command := exec.Command(executable, SuperviseCommand, "--dir", dir.Path())
	command.Stdin = empty
	command.Stdout = output
	command.Stderr = output
	// A session of its own, so that the supervisor keeps neither a terminal
	// nor a parent that could take it down with them.
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := command.Start(); err != nil {
		return fmt.Errorf("starting the supervisor: %w", err)
	}
	// Nothing waits for the supervisor, so hand it to init rather than leaving
	// a zombie behind when it finishes.
	return command.Process.Release()
}

func (l *DetachedLauncher) executable() (string, error) {
	if l.Executable != "" {
		return l.Executable, nil
	}

	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("finding this program to re-run as a supervisor: %w", err)
	}
	return executable, nil
}
