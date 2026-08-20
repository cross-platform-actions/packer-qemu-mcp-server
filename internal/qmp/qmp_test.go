package qmp

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/cross-platform-actions/packer-qemu-mcp-server/internal/qmptest"
)

func TestQueryStatusReadsTheGuestState(t *testing.T) {
	fake := qmptest.Start(t)
	fake.Reply("query-status", map[string]any{"status": "paused", "running": false})

	status, err := New(fake.Socket()).Status()

	require.NoError(t, err)
	require.Equal(t, "paused", status.Status)
	require.False(t, status.Running)
}

func TestCommandsNegotiateCapabilitiesFirst(t *testing.T) {
	fake := qmptest.Start(t)

	_, err := New(fake.Socket()).Status()

	require.NoError(t, err)
	require.Equal(t, []string{"qmp_capabilities", "query-status"}, fake.Executed())
}

func TestRepliesAreReadPastAsynchronousEvents(t *testing.T) {
	fake := qmptest.Start(t)
	fake.EventBefore("query-status", "RESUME")
	fake.Reply("query-status", map[string]any{"status": "running", "running": true})

	status, err := New(fake.Socket()).Status()

	require.NoError(t, err)
	require.Equal(t, "running", status.Status)
}

func TestScreendumpAsksForPNG(t *testing.T) {
	fake := qmptest.Start(t)

	require.NoError(t, New(fake.Socket()).Screendump("/tmp/shot.png"))

	require.Equal(t, map[string]any{
		"filename": "/tmp/shot.png",
		"format":   "png",
	}, fake.Arguments("screendump"))
}

func TestStopAndContAreSentVerbatim(t *testing.T) {
	fake := qmptest.Start(t)
	client := New(fake.Socket())

	require.NoError(t, client.Stop())
	require.NoError(t, client.Cont())

	require.Equal(t, []string{
		"qmp_capabilities", "stop",
		"qmp_capabilities", "cont",
	}, fake.Executed())
}

func TestErrorsFromQEMUAreReported(t *testing.T) {
	fake := qmptest.Start(t)
	fake.Fail("screendump", "GenericError", "no vga device")

	err := New(fake.Socket()).Screendump("/tmp/shot.png")

	require.ErrorContains(t, err, "no vga device")
}

func TestMissingSocketIsReportedAsUnavailable(t *testing.T) {
	client := New(filepath.Join(t.TempDir(), "nothing.sock"))

	_, err := client.Status()

	require.ErrorIs(t, err, ErrUnavailable)
}

func TestSilentQEMUTimesOut(t *testing.T) {
	fake := qmptest.StartSilent(t)
	client := New(fake.Socket())
	client.Timeout = 100 * time.Millisecond

	_, err := client.Status()

	require.ErrorContains(t, err, "timeout")
}

func TestConcurrentClientsTakeTurns(t *testing.T) {
	fake := qmptest.Start(t)
	client := New(fake.Socket())
	done := make(chan error, 8)

	for range cap(done) {
		go func() {
			_, err := client.Status()
			done <- err
		}()
	}

	for range cap(done) {
		require.NoError(t, <-done)
	}
	require.Equal(t, 8, fake.Connections())

	// One session is a handshake and one command. Any two of those adjacent to
	// each other would mean two clients were mid-session at once, which is one
	// more than QEMU serves.
	var expected []string
	for range cap(done) {
		expected = append(expected, "qmp_capabilities", "query-status")
	}
	require.Equal(t, expected, fake.Executed())
}

func TestSendKeysPressesEveryKeyOfAChordTogether(t *testing.T) {
	fake := qmptest.Start(t)

	require.NoError(t, New(fake.Socket()).SendKeys([]string{"ctrl", "alt", "f2"}, 0))

	require.Equal(t, map[string]any{"keys": []any{
		map[string]any{"type": "qcode", "data": "ctrl"},
		map[string]any{"type": "qcode", "data": "alt"},
		map[string]any{"type": "qcode", "data": "f2"},
	}}, fake.Arguments("send-key"))
}

func TestSendKeysCarriesAHoldTimeInMilliseconds(t *testing.T) {
	fake := qmptest.Start(t)

	require.NoError(t, New(fake.Socket()).SendKeys([]string{"ret"}, 150*time.Millisecond))

	require.Equal(t, float64(150), fake.Arguments("send-key")["hold-time"])
}

// The pointer is placed before the button is pressed, in one event list, so
// that nothing can move it in between.
func TestClickPlacesThePointerBeforePressing(t *testing.T) {
	fake := qmptest.Start(t)

	require.NoError(t, New(fake.Socket()).Click(0.5, 0, "left", 1))

	require.Equal(t, []any{
		map[string]any{"type": "abs", "data": map[string]any{"axis": "x", "value": float64(0x3FFF)}},
		map[string]any{"type": "abs", "data": map[string]any{"axis": "y", "value": float64(0)}},
		map[string]any{"type": "btn", "data": map[string]any{"down": true, "button": "left"}},
		map[string]any{"type": "btn", "data": map[string]any{"down": false, "button": "left"}},
	}, fake.Arguments("input-send-event")["events"])
}

func TestADoubleClickIsTwoPressesWithoutMovingBetweenThem(t *testing.T) {
	fake := qmptest.Start(t)

	require.NoError(t, New(fake.Socket()).Click(1, 1, "left", 2))

	events, ok := fake.Arguments("input-send-event")["events"].([]any)
	require.True(t, ok)
	require.Len(t, events, 6)
	require.Equal(t, map[string]any{"type": "abs", "data": map[string]any{"axis": "x", "value": float64(0x7FFF)}}, events[0])
}
