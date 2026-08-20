// Package packertest builds the shell stand-ins for `packer build -debug` that
// the tests run instead of Packer.
//
// A stand-in's only real job is the pause: print the prompt Packer prints, and
// then block on the terminal until enter arrives. Both halves of that are
// easier to get wrong than they look, so they live here rather than in each
// test that needs one.
package packertest

// rawMode leaves canonical mode, so that a single keystroke is delivered as
// soon as it is typed, the way Packer reads its prompt.
//
// It runs before the prompt is printed, and not after: the supervisor answers
// the instant it scans the prompt, which on an emulated guest is long before
// another fork and exec could finish. A keystroke that arrived while the line
// was still canonical would be sitting in a queue that is then reconfigured
// underneath it, and POSIX leaves the fate of queued input unspecified across
// that change.
const rawMode = `stty -icanon min 1 time 0 2>/dev/null`

// awaitAnswer blocks until one byte arrives from the terminal, and fails the
// stand-in if the terminal went away instead.
//
// The byte is counted rather than trusted: dd reports success after copying
// nothing at all, so a script that lost its terminal would otherwise run
// straight on as though the pause had been answered — which is exactly what
// the tests that assert a build stopped are looking for.
const awaitAnswer = `[ $(dd bs=1 count=1 2>/dev/null | wc -c) -eq 1 ] || exit 1`

// Pause is a pause prompt printed the way Packer prints it, followed by the
// wait Packer waits. The prompt is the text passed to printf, escapes and all.
func Pause(prompt string) string {
	return rawMode + `; printf "` + prompt + `"; ` + awaitAnswer
}

// BootStepPause is the pause after a labelled step of a boot command, which is
// the one an agent is meant to see.
func BootStepPause(label string) string {
	return Pause(`==> qemu.probe: Pausing after run of step 'boot description: \"` + label +
		`\", command: <enter>'. Press enter to continue. `)
}
