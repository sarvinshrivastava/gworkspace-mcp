package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/drive"
)

var bareRefreshRE = regexp.MustCompile(`^1//[\w.-]+$`)

// normaliseToken mirrors the TS: a bare refresh token is wrapped into a
// token.json document (with the warning), a document must hold a string
// refresh_token.
func normaliseToken(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if bareRefreshRE.MatchString(value) {
		fmt.Println("[auth] GOOGLE_TOKEN_JSON held a bare refresh token, not a token.json document — wrapping it. Store the whole file to silence this.")
		out, _ := json.Marshal(map[string]string{"refresh_token": value})
		return string(out) + "\n", nil
	}
	var parsed struct {
		RefreshToken any `json:"refresh_token"`
	}
	if err := json.Unmarshal([]byte(value), &parsed); err != nil {
		return "", fmt.Errorf("GOOGLE_TOKEN_JSON is neither a bare refresh token nor a token.json document: %v", err)
	}
	if s, ok := parsed.RefreshToken.(string); ok && s != "" {
		return value, nil
	}
	return "", fmt.Errorf("GOOGLE_TOKEN_JSON has no refresh_token string")
}

// refreshTokenOf reads the refresh_token out of a normalised document.
func refreshTokenOf(normalisedJSON string) string {
	var parsed struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal([]byte(normalisedJSON), &parsed); err != nil {
		return ""
	}
	return parsed.RefreshToken
}

// probeResultOf maps the boot probe's outcome onto the health machine: a
// rejection (invalid_grant and friends) is BrokenRejected, a transient
// failure is just not-OK, success clears.
func probeResultOf(err error) drive.ProbeResult {
	if err == nil {
		return drive.ProbeResult{OK: true}
	}
	var de *drive.Error
	if errors.As(err, &de) && de.Rejection != "" {
		// TS applyProbe: rejected iff the surfaced text names invalid_grant.
		// Other OAuth answers (invalid_client for a bad client pair, HTTP 401
		// with no category) are probe failures that do NOT move the health
		// state to rejected.
		detail := drive.RejectionDetail(de)
		return drive.ProbeResult{Rejected: strings.Contains(detail, "invalid_grant"), Detail: detail}
	}
	return drive.ProbeResult{OK: false, Detail: err.Error()}
}
