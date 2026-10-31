package policy

import "unicode/utf16"

// JavaScript strings measure and slice in UTF-16 code units. TS used
// value.length and value.slice(0, 80); Go's len and [:80] count bytes, which
// diverges on multibyte input (a 40-emoji name is 80 units but 160 bytes).
// These helpers reproduce the JS semantics exactly so the same inputs are
// accepted, rejected and truncated as the TS service.

// utf16Len is JavaScript's string.length.
func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

// utf16Slice is JavaScript's string.slice(0, n).
func utf16Slice(s string, n int) string {
	units := utf16.Encode([]rune(s))
	if len(units) <= n {
		return s
	}
	return string(utf16.Decode(units[:n]))
}
