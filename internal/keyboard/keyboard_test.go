package keyboard

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTypingAWordIsOnePressPerLetter(t *testing.T) {
	presses, err := Type("ls")

	require.NoError(t, err)
	require.Equal(t, []Press{{"l"}, {"s"}}, presses)
}

// A capital is the shift key and the letter held together, not the shift key
// pressed and released before it: the emulator presses everything it is given
// at once, and a shift that has already been let go shifts nothing.
func TestACapitalHoldsShiftWithTheLetter(t *testing.T) {
	presses, err := Type("Ok")

	require.NoError(t, err)
	require.Equal(t, []Press{{"shift", "o"}, {"k"}}, presses)
}

func TestTypingPunctuationAndWhitespace(t *testing.T) {
	presses, err := Type("/dev a\n")

	require.NoError(t, err)
	require.Equal(t, []Press{{"slash"}, {"d"}, {"e"}, {"v"}, {"spc"}, {"a"}, {"ret"}}, presses)
}

func TestShiftedPunctuationIsTheUnshiftedKeyWithShift(t *testing.T) {
	presses, err := Type("~$?")

	require.NoError(t, err)
	require.Equal(t, []Press{{"shift", "grave_accent"}, {"shift", "4"}, {"shift", "slash"}}, presses)
}

// Refusing to type a character is better than dropping it: a boot command that
// silently lost a character is the failure this whole server exists to find.
func TestTypingRefusesACharacterWithNoKey(t *testing.T) {
	_, err := Type("café")

	require.ErrorContains(t, err, "cannot type")
	require.ErrorContains(t, err, "é")
}

func TestKeysTakeTheirFriendlyNames(t *testing.T) {
	presses, err := Keys([]string{"down", "Enter", "pageup"})

	require.NoError(t, err)
	require.Equal(t, []Press{{"down"}, {"ret"}, {"pgup"}}, presses)
}

func TestACombinationIsOnePressOfEveryKeyInIt(t *testing.T) {
	presses, err := Keys([]string{"ctrl+alt+f2"})

	require.NoError(t, err)
	require.Equal(t, []Press{{"ctrl", "alt", "f2"}}, presses)
}

func TestKeysRefuseANameThatIsNotAKey(t *testing.T) {
	_, err := Keys([]string{"anykey"})

	require.ErrorContains(t, err, `no such key "anykey"`)
}
