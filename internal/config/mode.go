package config

import "fmt"

// Mode says how open_pr delivers a fix, from least to most risky:
//
//	safe  PR on a heraion/... branch, only for non-breaking changes
//	full  PR on a heraion/... branch, for any change (default)
//	yolo  commit pushed to the default branch; a PR if the push is rejected
type Mode string

// Modes.
const (
	ModeSafe Mode = "safe"
	ModeFull Mode = "full"
	ModeYolo Mode = "yolo"
)

// risk orders the modes, e.g. risk[ModeSafe] < risk[ModeYolo].
var risk = map[Mode]int{ModeSafe: 0, ModeFull: 1, ModeYolo: 2}

// Valid reports whether m is safe, full or yolo.
func (m Mode) Valid() bool {
	_, ok := risk[m]
	return ok
}

// Safer returns the less risky of a and b, e.g. Safer(yolo, full) = full.
func Safer(a, b Mode) Mode {
	if risk[a] <= risk[b] {
		return a
	}
	return b
}

// ValidateMode reports an unknown mode, e.g. "yolo!" for key "mode".
func ValidateMode(key string, m Mode) error {
	if !m.Valid() {
		return fmt.Errorf("%s %q must be %s, %s or %s", key, m, ModeSafe, ModeFull, ModeYolo)
	}
	return nil
}
