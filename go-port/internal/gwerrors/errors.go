// Package gwerrors: error text helpers shared across the service.
// Port of src/errors.ts.
package gwerrors

import "regexp"

// ErrorMessage is the message of anything thrown, for a log line or a
// user-facing failure.
func ErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Secretish matches text that looks like a credential. Never logged: a line
// matching it is dropped or replaced.
var Secretish = regexp.MustCompile(`(?i)refresh_token|client_secret|access_token|Bearer\s|"token"`)

var controlChars = regexp.MustCompile("[\x00-\x1f\x7f]")

// RedactDetail makes text log-only: credential-shaped text is replaced,
// control characters flattened, the rest capped.
func RedactDetail(detail string, maxLen int) string {
	if Secretish.MatchString(detail) {
		return "[redacted]"
	}
	flat := controlChars.ReplaceAllString(detail, " ")
	if maxLen > 0 {
		// TS .slice(0, max) counts UTF-16 code units, not bytes: a byte slice
		// could split a multibyte rune and under-counts astral characters.
		flat = utf16Head(flat, maxLen)
	}
	return flat
}

// utf16Head returns the longest prefix of s within n UTF-16 code units. A
// boundary mid-surrogate-pair keeps the whole pair in Go (a Go string cannot
// hold a lone surrogate); JS would keep the lead surrogate. Documented
// compatibility edge, same as the label slicing in policy.
func utf16Head(s string, n int) string {
	units := 0
	for i, r := range s {
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		if units+w > n {
			return s[:i]
		}
		units += w
	}
	return s
}
