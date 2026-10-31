package tools

import "github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/enforce"

// parseJSDateToRFC3339 mirrors new Date(v).toISOString() in the upstream
// tools (listDriveFiles.modifiedAfter, calendar time bounds).
func parseJSDateToRFC3339(value string) (string, error) {
	return enforce.ParseJSDateISO(value)
}
