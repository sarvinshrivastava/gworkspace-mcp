package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
)

// Slice-local argument helpers extending the shared Args contract
// (internal/tools/tools.go is core-owned; these live here to avoid touching
// shared files). Message conventions follow the exemplar.

// argOptNumber reads an optional z.number() WITHOUT .int() — e.g.
// readDocument.maxLength. present reports whether the key was supplied.
func (a *Args) argOptNumber(key string) (float64, bool, error) {
	r, ok := a.raw[key]
	if !ok {
		return 0, false, nil
	}
	f, err := strconv.ParseFloat(string(r), 64)
	if err != nil {
		return 0, false, &UserError{Msg: fmt.Sprintf("Expected number for \"%s\"", key)}
	}
	return f, true, nil
}

// argOptBoolDefault reads an optional boolean with a non-false zod default.
func (a *Args) argOptBoolDefault(key string, def bool) (bool, error) {
	if !a.has(key) {
		return def, nil
	}
	return a.OptBool(key)
}

// argReqIntMin reads a required z.number().int().min(min).
func (a *Args) argReqIntMin(key string, min int) (int, error) {
	r, ok := a.raw[key]
	if !ok {
		return 0, &UserError{Msg: fmt.Sprintf("Required: \"%s\"", key)}
	}
	f, err := strconv.ParseFloat(string(r), 64)
	if err != nil {
		return 0, &UserError{Msg: fmt.Sprintf("Expected number for \"%s\"", key)}
	}
	if f != math.Trunc(f) {
		return 0, &UserError{Msg: fmt.Sprintf("Expected integer for \"%s\"", key)}
	}
	v := int(f)
	if v < min {
		return 0, &UserError{Msg: fmt.Sprintf("Number must be greater than or equal to %d: \"%s\"", min, key)}
	}
	return v, nil
}

// jsSubstr is JavaScript's s.substring(0, n): n is truncated toward zero and
// clamped to [0, len]; the slice counts UTF-16 code units.
func jsSubstr(s string, n float64) string {
	i := int(math.Trunc(n))
	if i < 0 {
		i = 0
	}
	return utf16Slice(s, i)
}

// jsNum formats a float the way JS String(number) does for the magnitudes
// involved here (integers without a decimal point).
func jsNum(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1e21 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// wrapFailed mirrors the upstream catch pattern: UserError (and other typed
// passthroughs) propagate unchanged; anything else becomes
// `Failed to <what>: <error.message || 'Unknown error'>`.
func wrapFailed(what string, err error) error {
	var ue *UserError
	if errors.As(err, &ue) {
		return err
	}
	var mce *MarkdownConversionError
	if errors.As(err, &mce) {
		return err
	}
	msg := err.Error()
	if msg == "" {
		msg = "Unknown error"
	}
	return &UserError{Msg: "Failed to " + what + ": " + msg}
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// httpErrorText is the message a googleapis HTTP error surfaces as
// error.message — gaxios's extraction, shared ErrMessage.
func httpErrorText(body []byte, status int) string { return ErrMessage(body, status) }

// httpErrorCode is error.response?.data?.error?.code || error.code: the
// body's numeric error code when present, else the HTTP status.
func httpErrorCode(body []byte, status int) int {
	var parsed struct {
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if jsonUnmarshal(body, &parsed) == nil && parsed.Error.Code != 0 {
		return parsed.Error.Code
	}
	return status
}
