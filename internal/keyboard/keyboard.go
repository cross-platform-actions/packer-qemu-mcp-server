// Package keyboard turns text and key names into what QEMU's monitor accepts.
//
// The monitor takes key codes — its own names for the physical keys of a US
// keyboard — and presses whatever it is given all at once. Typing a word is
// therefore a series of one-key presses, and a shifted character is a press of
// two keys together, which is also how a chord such as ctrl+alt+f2 is sent.
package keyboard

import (
	"fmt"
	"strings"
)

// A Press is the set of keys held down together, and then released together.
type Press []string

// shift is the key that makes a character its shifted self.
const shift = "shift"

// Type returns the presses that type the given text on a US keyboard, which is
// the layout the emulator assumes until a guest is far enough into its
// installation to have been told otherwise.
func Type(text string) ([]Press, error) {
	presses := make([]Press, 0, len(text))
	for _, character := range text {
		press, err := typed(character)
		if err != nil {
			return nil, err
		}
		presses = append(presses, press)
	}
	return presses, nil
}

// Keys returns the presses named by a list such as ["down", "down", "ret"]. A
// name may be a combination joined by +, whose keys are pressed together.
func Keys(names []string) ([]Press, error) {
	presses := make([]Press, 0, len(names))
	for _, name := range names {
		press, err := combination(name)
		if err != nil {
			return nil, err
		}
		presses = append(presses, press)
	}
	return presses, nil
}

func combination(name string) (Press, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("an empty key name")
	}

	press := make(Press, 0, 3)
	for _, part := range strings.Split(name, "+") {
		code, known := named[strings.ToLower(strings.TrimSpace(part))]
		if !known {
			return nil, fmt.Errorf("no such key %q", part)
		}
		press = append(press, code)
	}
	return press, nil
}

// typed is the press that types one character.
func typed(character rune) (Press, error) {
	if code, plain := unshifted[character]; plain {
		return Press{code}, nil
	}
	if code, needsShift := shifted[character]; needsShift {
		return Press{shift, code}, nil
	}
	return nil, fmt.Errorf("cannot type %q: only the printable characters of a US keyboard, "+
		"tab and newline can be typed, and anything else has to be named as a key", character)
}

// unshifted are the characters that are a key on their own.
var unshifted = map[rune]string{
	' ': "spc", '\t': "tab", '\n': "ret", '\r': "ret",
	'-': "minus", '=': "equal", '[': "bracket_left", ']': "bracket_right",
	'\\': "backslash", ';': "semicolon", '\'': "apostrophe", '`': "grave_accent",
	',': "comma", '.': "dot", '/': "slash",
}

// shifted are the characters that need the shift key held with them.
var shifted = map[rune]string{
	'_': "minus", '+': "equal", '{': "bracket_left", '}': "bracket_right",
	'|': "backslash", ':': "semicolon", '"': "apostrophe", '~': "grave_accent",
	'<': "comma", '>': "dot", '?': "slash",
	'!': "1", '@': "2", '#': "3", '$': "4", '%': "5",
	'^': "6", '&': "7", '*': "8", '(': "9", ')': "0",
}

// named are the keys that can be asked for by name, including the friendlier
// spellings of the ones whose key code is an abbreviation.
var named = map[string]string{
	"enter": "ret", "return": "ret", "ret": "ret",
	"space": "spc", "spacebar": "spc", "spc": "spc",
	"esc": "esc", "escape": "esc", "tab": "tab",
	"backspace": "backspace", "bs": "backspace",
	"delete": "delete", "del": "delete", "insert": "insert", "ins": "insert",
	"up": "up", "down": "down", "left": "left", "right": "right",
	"home": "home", "end": "end",
	"pageup": "pgup", "pgup": "pgup", "pagedown": "pgdn", "pgdn": "pgdn",
	"shift": "shift", "leftshift": "shift", "rightshift": "shift_r",
	"ctrl": "ctrl", "control": "ctrl", "leftctrl": "ctrl", "rightctrl": "ctrl_r",
	"alt": "alt", "leftalt": "alt", "rightalt": "alt_r",
	"meta": "meta_l", "super": "meta_l", "leftsuper": "meta_l", "rightsuper": "meta_r",
	"command": "meta_l", "leftcommand": "meta_l", "rightcommand": "meta_r",
	"option": "alt", "leftoption": "alt", "rightoption": "alt_r",
	"menu": "menu", "print": "print", "sysrq": "sysrq", "pause": "pause",
	"capslock": "caps_lock", "numlock": "num_lock", "scrolllock": "scroll_lock",
	"minus": "minus", "equal": "equal", "comma": "comma", "dot": "dot",
	"slash": "slash", "backslash": "backslash", "semicolon": "semicolon",
	"apostrophe": "apostrophe", "grave": "grave_accent",
	"bracketleft": "bracket_left", "bracketright": "bracket_right",
	"kpenter": "kp_enter", "kpadd": "kp_add", "kpsubtract": "kp_subtract",
	"kpmultiply": "kp_multiply", "kpdivide": "kp_divide", "kpdecimal": "kp_decimal",
}

// The letters, digits and function keys follow a pattern, and a table of them
// written out by hand is a table with a typo in it.
func init() {
	for character := 'a'; character <= 'z'; character++ {
		code := string(character)
		unshifted[character] = code
		shifted[character-'a'+'A'] = code
		named[code] = code
	}
	for character := '0'; character <= '9'; character++ {
		code := string(character)
		unshifted[character] = code
		named[code] = code
	}
	for number := 1; number <= 12; number++ {
		code := fmt.Sprintf("f%d", number)
		named[code] = code
	}
	for number := 0; number <= 9; number++ {
		named[fmt.Sprintf("kp%d", number)] = fmt.Sprintf("kp_%d", number)
	}
}
