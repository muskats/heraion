// Package textutil holds small string helpers shared by the tools.
package textutil

import "unicode/utf8"

// Truncate returns at most n bytes of s, cut on a rune boundary so the
// result stays valid UTF-8, e.g. Truncate("héllo", 2) = "h".
func Truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
