package config

import "fmt"

// Mode says how open_pr delivers a fix, from least to most risky:
//
//	safe  PR on a heraion/... branch, only for non-breaking changes
//	full  PR on a heraion/... branch, for any change (default)
type Mode string

// Modes.
const (
	ModeSafe Mode = "safe"
	ModeFull Mode = "full"
)

// removedYolo is the mode that pushed to the default branch, removed in 3.0.0.
const removedYolo Mode = "yolo"

// ErrYoloRemoved is the error for mode "yolo".
const ErrYoloRemoved = `mode "yolo" was removed in 3.0.0; use "full" (pull requests). See docs/upgrading-3.md`

// risk orders the modes, e.g. risk[ModeSafe] < risk[ModeFull].
var risk = map[Mode]int{ModeSafe: 0, ModeFull: 1}

// Valid reports whether m is safe or full.
func (m Mode) Valid() bool {
	_, ok := risk[m]
	return ok
}

// Safer returns the less risky of a and b, e.g. Safer(full, safe) = safe.
func Safer(a, b Mode) Mode {
	if risk[a] <= risk[b] {
		return a
	}
	return b
}

// ValidateMode reports an unknown mode, e.g. "reckless" for key "mode".
func ValidateMode(key string, m Mode) error {
	switch {
	case m == removedYolo:
		return fmt.Errorf("%s: %s", key, ErrYoloRemoved)
	case !m.Valid():
		return fmt.Errorf("%s %q must be %s or %s", key, m, ModeSafe, ModeFull)
	}
	return nil
}
