package supervisor

import (
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The whole harness rests on one property of the terminal it hands a build: the
// build waits until a keystroke is written to this end, and then reads it.
// Packer's pause is nothing more than that, and a platform where it does not
// hold cannot be supported — so it is asserted here rather than inferred from
// the behaviour of a build, and a failure carries the terminal's own settings,
// which is the thing one needs to know.
//
// What the build reads is not necessarily what was written: whether a carriage
// return arrives as one, or as a newline, is up to the line discipline, and the
// platforms disagree. That is the reader's business, not the harness's.
func TestTheTerminalCarriesAKeystrokeToTheBuild(t *testing.T) {
	// Raw mode first, and only then the marker that says the read is next: the
	// settings the read happens under are the ones worth printing, and nothing
	// may be written to this end before they are in force.
	command := exec.Command("/bin/sh", "-c", `
stty -icanon min 1 time 0 2>&1
stty -a 2>&1
printf 'ready\n'
dd bs=1 count=1 2>/dev/null | od -c
printf 'answered\n'
`)

	terminal, err := Open(command)
	require.NoError(t, err)
	defer func() {
		// Closing this end first ends the read, so that a test which fails
		// before it answers does not then wait on a shell blocked forever.
		terminal.Close()
		terminal.Wait()
	}()

	said := &transcript{terminal: terminal}
	go said.read()

	require.True(t, said.await("ready", patience),
		"the build never got as far as reading:\n%s", said.text())

	// Nothing may arrive before the answer is sent, or the pause is no pause.
	require.False(t, said.await("answered", 2*time.Second),
		"the build read something before anything was written:\n%s", said.text())

	require.NoError(t, terminal.Answer())

	require.True(t, said.await("answered", patience),
		"the keystroke never arrived:\n%s", said.text())

	// od prints the one byte that arrived, whichever of the two it is.
	arrived := said.text()
	require.True(t, strings.Contains(arrived, `\r`) || strings.Contains(arrived, `\n`),
		"a byte arrived that was neither a carriage return nor a newline:\n%s", arrived)
}

// transcript is everything the build has said so far.
type transcript struct {
	terminal *Terminal

	mutex sync.Mutex
	whole strings.Builder
}

func (s *transcript) read() {
	buffer := make([]byte, 512)
	for {
		read, err := s.terminal.Read(buffer)
		if read > 0 {
			s.mutex.Lock()
			s.whole.Write(buffer[:read])
			s.mutex.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (s *transcript) await(what string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if strings.Contains(s.text(), what) {
			return true
		}
		time.Sleep(tick)
	}
	return false
}

func (s *transcript) text() string {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.whole.String()
}
