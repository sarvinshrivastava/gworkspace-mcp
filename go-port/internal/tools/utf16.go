package tools

import "unicode/utf16"

// JavaScript strings measure and slice in UTF-16 code units. The upstream
// handlers use String.length, substring and slice for indices and limits, so
// all index arithmetic and truncation here uses UTF-16 units, not bytes.
// (Same semantics as internal/policy's unexported utf16 helpers; duplicated
// because the tools package cannot reach unexported policy symbols.)

// utf16Len is JavaScript's string.length.
func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

// utf16Slice is JavaScript's string.slice(0, n) / substring(0, n).
func utf16Slice(s string, n int) string {
	if n <= 0 {
		return ""
	}
	units := utf16.Encode([]rune(s))
	if len(units) <= n {
		return s
	}
	return string(utf16.Decode(units[:n]))
}
