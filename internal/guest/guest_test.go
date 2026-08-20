package guest

import (
	"image"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/cross-platform-actions/packer-qemu-mcp-server/internal/keyboard"
	"github.com/cross-platform-actions/packer-qemu-mcp-server/internal/qmptest"
)

func TestFreezingAndResumingAreOnlyEverSentOnce(t *testing.T) {
	fake := qmptest.Start(t)
	watched := At(fake.Socket())

	// A guest that is already running is not resumed again: the monitor would
	// take it, but the harness would be saying something it does not mean.
	require.NoError(t, watched.Resume())
	require.NoError(t, watched.Freeze())
	require.True(t, watched.Frozen())
	require.NoError(t, watched.Resume())
	require.False(t, watched.Frozen())

	require.Equal(t, []string{"stop", "cont"}, changes(fake.Executed()))
}

func TestConditionIsWhatTheEmulatorSays(t *testing.T) {
	fake := qmptest.Start(t)
	fake.Reply("query-status", map[string]any{"status": "paused", "running": false})

	condition, err := At(fake.Socket()).Condition()

	require.NoError(t, err)
	require.Equal(t, "paused", condition.Name)
	require.False(t, condition.Running)
}

func TestPhotographingMeasuresWhatItTook(t *testing.T) {
	fake := qmptest.Start(t)
	fake.Draw(1024, 768)
	path := filepath.Join(t.TempDir(), "screen.png")

	screen, err := At(fake.Socket()).Photograph(path)

	require.NoError(t, err)
	require.Equal(t, Screen{Path: path, Width: 1024, Height: 768}, screen)
}

func TestAScreenKnowsWhichPointsAreOnIt(t *testing.T) {
	screen := Screen{Width: 800, Height: 600}

	require.True(t, screen.Holds(image.Pt(0, 0)))
	require.True(t, screen.Holds(image.Pt(799, 599)))
	require.False(t, screen.Holds(image.Pt(800, 599)), "one past the right edge")
	require.False(t, screen.Holds(image.Pt(0, -1)))
}

// The tablet reports where it is as a fraction of the screen, so the same
// point on a larger screen is the same fraction.
func TestAPointIsSentAsAFractionOfTheScreen(t *testing.T) {
	fake := qmptest.Start(t)

	require.NoError(t, At(fake.Socket()).Point(Screen{Width: 800, Height: 600}, image.Pt(400, 0), "left", 1))

	events, ok := fake.Arguments("input-send-event")["events"].([]any)
	require.True(t, ok)
	require.Equal(t,
		map[string]any{"type": "abs", "data": map[string]any{"axis": "x", "value": float64(0x3FFF)}},
		events[0])
}

func TestTypingSendsOnePressAtATime(t *testing.T) {
	fake := qmptest.Start(t)

	require.NoError(t, At(fake.Socket()).Type([]keyboard.Press{{"l"}, {"shift", "s"}}, 0, time.Millisecond))

	require.Equal(t, 2, sent(fake.Executed(), "send-key"))
}

func changes(executed []string) []string {
	var found []string
	for _, was := range executed {
		if was == "stop" || was == "cont" {
			found = append(found, was)
		}
	}
	return found
}

func sent(executed []string, command string) int {
	count := 0
	for _, was := range executed {
		if was == command {
			count++
		}
	}
	return count
}
