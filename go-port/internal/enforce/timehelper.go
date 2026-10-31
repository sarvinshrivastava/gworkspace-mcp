package enforce

import (
	"errors"
	"time"
)

type time0 = time.Time

var errInvalidTime = errors.New("Invalid time value")

func timeParse(layout, value string) (time.Time, error) {
	return time.Parse(layout, value)
}

// ParseJSDateISO is new Date(value).toISOString(): parse the shapes the JS
// Date constructor reliably accepts, then format as UTC with milliseconds —
// the exact string the upstream splices into Drive queries and Calendar
// timeMin/timeMax.
func ParseJSDateISO(value string) (string, error) {
	t, err := parseJSDate(value)
	if err != nil {
		return "", err
	}
	return t.UTC().Format("2006-01-02T15:04:05.000Z"), nil
}
