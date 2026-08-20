package pause

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Transcripts below are verbatim from a real `packer build -debug` run of the
// qemu builder over a PTY, including the missing trailing newline: Packer
// prints the prompt and then blocks on the terminal without ending the line.
const (
	downloadPrompt  = "==> qemu.probe: Pausing after run of step 'StepDownload'. Press enter to continue. "
	bootStepPrompt  = "==> qemu.probe: Pausing after run of step 'boot description: \"second step\", command: <enter>'. Press enter to continue. "
	bootCmdPrompt   = "==> qemu.probe: Pausing after run of step 'boot_command: <enter>'. Press enter to continue. "
	bootDonePrompt  = "==> qemu.probe: Pausing after run of step 'stepTypeBootCommand'. Press enter to continue. "
	cleanupPrompt   = "==> qemu.probe: Pausing before cleanup of step 'stepRun'. Press enter to continue. "
	connectPrompt   = "==> qemu.probe: Pausing after run of step 'StepConnect'. Press enter to continue. "
	ordinaryLogLine = "==> qemu.probe: Waiting 1s for boot...\r\n"
)

func TestScanFindsPauseWithoutTrailingNewline(t *testing.T) {
	var s Scanner

	pauses := s.Scan([]byte(downloadPrompt))

	require.Len(t, pauses, 1)
	require.Equal(t, AfterRun, pauses[0].Location)
	require.Equal(t, "StepDownload", pauses[0].Step)
}

func TestScanIgnoresOrdinaryOutput(t *testing.T) {
	var s Scanner

	require.Empty(t, s.Scan([]byte(ordinaryLogLine)))
}

func TestScanFindsPauseSplitAcrossChunks(t *testing.T) {
	var s Scanner
	split := len(downloadPrompt) / 2

	require.Empty(t, s.Scan([]byte(downloadPrompt[:split])))

	pauses := s.Scan([]byte(downloadPrompt[split:]))

	require.Len(t, pauses, 1)
	require.Equal(t, "StepDownload", pauses[0].Step)
}

func TestScanFindsSeveralPausesInOneChunk(t *testing.T) {
	var s Scanner

	// Answering a pause leaves no newline behind, so the next prompt lands on
	// the same line as the previous one.
	pauses := s.Scan([]byte(downloadPrompt + bootDonePrompt))

	require.Len(t, pauses, 2)
	require.Equal(t, "StepDownload", pauses[0].Step)
	require.Equal(t, "stepTypeBootCommand", pauses[1].Step)
}

func TestScanReportsTheSamePauseOnlyOnce(t *testing.T) {
	var s Scanner

	require.Len(t, s.Scan([]byte(downloadPrompt)), 1)
	require.Empty(t, s.Scan([]byte(ordinaryLogLine)))
}

func TestScanReadsCleanupLocation(t *testing.T) {
	var s Scanner

	pauses := s.Scan([]byte(cleanupPrompt))

	require.Len(t, pauses, 1)
	require.Equal(t, BeforeCleanup, pauses[0].Location)
	require.Equal(t, "stepRun", pauses[0].Step)
}

func TestScanMatchesRewordedPrompt(t *testing.T) {
	var s Scanner

	// Prompt wording has drifted across Packer versions; match it loosely.
	pauses := s.Scan([]byte("Pausing after run of step 'StepDownload'. Hit enter to keep going. "))

	require.Len(t, pauses, 1)
	require.Equal(t, "StepDownload", pauses[0].Step)
}

func TestScanFindsAPauseWhoseStepNameSpansLines(t *testing.T) {
	var s Scanner

	// A boot step's "name" is its description and keystrokes, and a heredoc
	// puts a newline in either. Missing the prompt would stop the build for
	// good, since nothing would ever answer it.
	pauses := s.Scan([]byte("Pausing after run of step 'boot description: \"install\",\ncommand: <enter>'. Press enter to continue. "))

	require.Len(t, pauses, 1)
	require.True(t, pauses[0].Interesting())
}

func TestScanKeepsBufferBounded(t *testing.T) {
	var s Scanner
	noise := make([]byte, 4096)
	for i := range noise {
		noise[i] = 'x'
	}

	for range 100 {
		require.Empty(t, s.Scan(noise))
	}

	require.LessOrEqual(t, s.buffered(), maxBuffer)
	require.Len(t, s.Scan([]byte(downloadPrompt)), 1)
}

func TestBootStepPauseIsInteresting(t *testing.T) {
	var s Scanner

	pauses := s.Scan([]byte(bootStepPrompt))

	require.True(t, pauses[0].Interesting())
	require.Equal(t, "second step", pauses[0].Label())
}

func TestUnlabelledBootStepPauseIsInteresting(t *testing.T) {
	var s Scanner

	pauses := s.Scan([]byte(bootCmdPrompt))

	require.True(t, pauses[0].Interesting())
	require.Equal(t, "boot_command: <enter>", pauses[0].Label())
}

func TestLastBootStepPauseIsInteresting(t *testing.T) {
	var s Scanner

	pauses := s.Scan([]byte(bootDonePrompt))

	require.True(t, pauses[0].Interesting())
}

func TestOrdinaryStepPauseIsNotInteresting(t *testing.T) {
	var s Scanner

	for _, prompt := range []string{downloadPrompt, connectPrompt} {
		pauses := s.Scan([]byte(prompt))
		require.False(t, pauses[0].Interesting(), prompt)
	}
}

func TestCleanupPauseIsNeverInteresting(t *testing.T) {
	var s Scanner

	// The guest is being torn down; freezing it there would hang the build.
	pauses := s.Scan([]byte("Pausing before cleanup of step 'stepTypeBootCommand'. Press enter to continue. "))

	require.False(t, pauses[0].Interesting())
}

func TestAPauseIsMatchedByPartOfItsLabel(t *testing.T) {
	var s Scanner
	found := s.Scan([]byte(bootStepPrompt))[0]

	// Labels come from the template, so naming a step should not mean
	// reproducing its punctuation exactly.
	require.True(t, found.Matches("second step"))
	require.True(t, found.Matches("SECOND"))
	require.False(t, found.Matches("first step"))
	require.False(t, found.Matches(""))
}

func TestTheEndOfTheBootCommandIsRecognisable(t *testing.T) {
	var s Scanner

	require.True(t, s.Scan([]byte(bootDonePrompt))[0].EndsBootCommand())
	require.False(t, s.Scan([]byte(bootStepPrompt))[0].EndsBootCommand())
	require.False(t, s.Scan([]byte(downloadPrompt))[0].EndsBootCommand())
}
