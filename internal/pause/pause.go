// Package pause recognises Packer's `-debug` pause prompts in the raw output
// of a build.
package pause

import (
	"regexp"
	"strings"
)

// Location is where in a step's lifecycle Packer paused.
type Location string

const (
	AfterRun      Location = "after run of"
	BeforeCleanup Location = "before cleanup of"
	Unknown       Location = "at"
)

// Pause is a Packer `-debug` prompt waiting for a keystroke.
type Pause struct {
	Location Location
	// Step is the step name exactly as Packer printed it. For an entry of
	// `boot_steps` it is not a step name at all but the entry's description
	// and keystrokes.
	Step string
}

// Interesting reports whether the guest should be frozen and photographed at
// this pause. Only the entries of `boot_steps` and the end of the boot command
// qualify: everything before them has no framebuffer to read, and everything
// after them is either inside `StepConnect`, whose ssh_timeout keeps ticking
// while the guest is frozen, or holding an established SSH session that a long
// freeze would kill.
func (p Pause) Interesting() bool {
	return p.Location == AfterRun && (p.bootStep() || p.EndsBootCommand())
}

// EndsBootCommand reports whether this is the pause after the whole boot
// command rather than after one entry of it: the last chance to look before
// the build starts waiting for SSH, and the backstop for anyone running the
// build forward to a step that never arrives.
func (p Pause) EndsBootCommand() bool {
	return p.Location == AfterRun && strings.EqualFold(p.Step, bootCommandStep)
}

// Matches reports whether this pause is the one a caller named. Labels are
// written in the template for people to read, so naming one is a matter of
// saying enough of it to be unambiguous, not of reproducing its punctuation.
func (p Pause) Matches(label string) bool {
	if label == "" {
		return false
	}
	return strings.Contains(strings.ToLower(p.Label()), strings.ToLower(label))
}

// Label is the human-readable description of a `boot_steps` entry, falling
// back to the step name for every other pause.
func (p Pause) Label() string {
	if match := describedBootStepRE.FindStringSubmatch(p.Step); match != nil {
		return match[1]
	}
	return p.Step
}

func (p Pause) bootStep() bool {
	step := strings.ToLower(p.Step)
	return strings.HasPrefix(step, "boot description:") || strings.HasPrefix(step, "boot_command:")
}

// Scanner extracts pauses from the byte stream of a build's terminal. The
// prompt is not newline-terminated — Packer prints it and blocks on the
// terminal — so the stream cannot be split into lines first.
type Scanner struct {
	tail []byte
}

// maxBuffer bounds the unmatched tail kept between calls. It only has to hold
// one prompt, and a prompt carries at most one boot command.
const maxBuffer = 8192

// bootCommandStep is the step that types every entry of `boot_steps`. Its own
// pause, after the last entry, is the highest-value screenshot of a build:
// it answers "did the whole sequence land?".
const bootCommandStep = "stepTypeBootCommand"

// Wording has drifted across Packer versions, so match the prompt loosely: the
// step name in quotes is the only part worth pinning.
//
// The name spans newlines because it is not always a step name: for an entry
// of `boot_steps` it is the entry's description and keystrokes, and either can
// carry a newline. Failing to match one would leave the prompt unanswered and
// the build stopped for good.
var (
	pauseRE             = regexp.MustCompile(`(?is)pausing\s+(after run of|before cleanup of|at)?\s*step\s+'(.*?)'\.`)
	describedBootStepRE = regexp.MustCompile(`(?i)^boot description:\s+"(.*)",\s+command:`)
)

// Scan reports the pauses in the next chunk of terminal output. A prompt split
// across chunks is reported once, when its last byte arrives.
func (s *Scanner) Scan(chunk []byte) []Pause {
	s.tail = append(s.tail, chunk...)

	var pauses []Pause
	for {
		match := pauseRE.FindSubmatchIndex(s.tail)
		if match == nil {
			break
		}
		pauses = append(pauses, Pause{
			Location: location(string(s.tail[match[2]:match[3]])),
			Step:     string(s.tail[match[4]:match[5]]),
		})
		s.tail = s.tail[match[1]:]
	}

	s.truncate()
	return pauses
}

func (s *Scanner) truncate() {
	if len(s.tail) > maxBuffer {
		s.tail = s.tail[len(s.tail)-maxBuffer:]
	}
}

func (s *Scanner) buffered() int {
	return len(s.tail)
}

func location(matched string) Location {
	switch strings.ToLower(matched) {
	case string(AfterRun):
		return AfterRun
	case string(BeforeCleanup):
		return BeforeCleanup
	default:
		return Unknown
	}
}
