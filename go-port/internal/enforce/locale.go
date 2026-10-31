package enforce

import "strings"

// localeCompare approximates JavaScript's String.prototype.localeCompare for
// the ASCII-dominant names Drive folders carry. Documented caveat in the TS
// source: "localeCompare ... can differ from Drive's collation at the
// margins" — the Go port inherits the same documented margin.
//
// TODO: exact ICU collation is not reproducible without a
// dependency; case-insensitive-then-codepoint ordering matches en-US ICU for
// plain ASCII names. Flagged in the design-delta list rather than silently
// settled.
func localeCompare(a, b string) int {
	la, lb := strings.ToLower(a), strings.ToLower(b)
	if la != lb {
		if la < lb {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

// parseJSDate accepts the date shapes JS's Date constructor reliably parses:
// RFC3339/ISO-8601 variants. Anything else is "Invalid time value", matching
// the upstream's RangeError.
func parseJSDate(value string) (t time0, err error) {
	for _, layout := range []string{
		"2006-01-02T15:04:05.999999999Z07:00",
		"2006-01-02T15:04:05Z0700",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		if parsed, perr := timeParse(layout, value); perr == nil {
			return parsed, nil
		}
	}
	return t, errInvalidTime
}
