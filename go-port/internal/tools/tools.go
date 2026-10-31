// Package tools holds the 20 exposed tool handlers. Nineteen are Go-native
// ports of the pinned upstream @a-bonus/google-docs-mcp@1.11.3 handlers (they
// were relayed to the Node child in the TS gateway); searchDriveFiles is
// served from internal/enforce. Parity rules and the
// exact-20 map live in PARITY.md at the module root.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// UserError mirrors fastmcp's UserError: the gateway turns it into a tool
// result with isError:true and this exact message as the text content.
type UserError struct{ Msg string }

func (e *UserError) Error() string { return e.Msg }

// Result is one tool call's wire result.
type Result struct {
	Text    string
	IsError bool
}

// Handler executes one tool call. args is the raw JSON arguments object AFTER
// the enforce pipeline's mutations; the handler applies the upstream zod
// schema's validation and defaults itself, because the MCP SDK applies none.
type Handler func(ctx context.Context, g *Google, args json.RawMessage) (string, error)

// Stringify is JSON.stringify(value, null, 2): two-space indent, no trailing
// newline, and NO HTML escaping (Go's default \u003c\u003e\u0026 escaping would
// drift from JS). Build outputs from structs or ordered writes — Go maps do
// not preserve JS insertion order.
func Stringify(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		panic(err) // handler outputs are always marshalable; a failure is a bug
	}
	// Go's encoder escapes U+2028/U+2029 even with HTML escaping off;
	// JSON.stringify emits them raw. Un-escape the two sequences — they can
	// only have come from the encoder.
	out := string(bytes.TrimRight(buf.Bytes(), "\n"))
	out = strings.ReplaceAll(out, "\\u2028", "\u2028")
	out = strings.ReplaceAll(out, "\\u2029", "\u2029")
	return out
}

// Args is a strict-decoding view of the raw arguments object, matching zod's
// strictObject: unknown keys reject the whole call.
type Args struct {
	raw     map[string]json.RawMessage
	allowed map[string]bool
}

// ParseArgs decodes args and rejects any key the tool's schema does not
// declare, in the zod strictObject spirit. allowed lists every schema key.
func ParseArgs(args json.RawMessage, allowed ...string) (*Args, error) {
	a := &Args{allowed: map[string]bool{}}
	for _, k := range allowed {
		a.allowed[k] = true
	}
	if len(args) == 0 {
		a.raw = map[string]json.RawMessage{}
		return a, nil
	}
	if err := json.Unmarshal(args, &a.raw); err != nil {
		return nil, &UserError{Msg: "invalid arguments: " + err.Error()}
	}
	for k := range a.raw {
		if !a.allowed[k] {
			return nil, &UserError{Msg: fmt.Sprintf("Unrecognized key: \"%s\"", k)}
		}
	}
	return a, nil
}

func (a *Args) has(key string) bool {
	_, ok := a.raw[key]
	return ok
}

// isExplicitNull: JSON null is a PRESENT value; zod's .optional() accepts
// only undefined, so an explicit null rejects like any other wrong type.
func isExplicitNull(r json.RawMessage) bool {
	return string(bytes.TrimSpace(r)) == "null"
}

// ReqString reads a required z.string(); min1 mirrors .min(1).
func (a *Args) ReqString(key string, min1 bool) (string, error) {
	r, ok := a.raw[key]
	if !ok {
		return "", &UserError{Msg: fmt.Sprintf("Required: \"%s\"", key)}
	}
	if isExplicitNull(r) {
		return "", &UserError{Msg: fmt.Sprintf("Expected string, received null: \"%s\"", key)}
	}
	var s string
	if err := json.Unmarshal(r, &s); err != nil {
		return "", &UserError{Msg: fmt.Sprintf("Expected string for \"%s\"", key)}
	}
	if min1 && s == "" {
		return "", &UserError{Msg: fmt.Sprintf("String must contain at least 1 character(s): \"%s\"", key)}
	}
	return s, nil
}

// OptString reads an optional string; absent → "".
func (a *Args) OptString(key string) (string, error) {
	if !a.has(key) {
		return "", nil
	}
	return a.ReqString(key, false)
}

// OptBool reads an optional boolean; absent → false.
func (a *Args) OptBool(key string) (bool, error) {
	r, ok := a.raw[key]
	if !ok {
		return false, nil
	}
	if isExplicitNull(r) {
		return false, &UserError{Msg: fmt.Sprintf("Expected boolean, received null: \"%s\"", key)}
	}
	var b bool
	if err := json.Unmarshal(r, &b); err != nil {
		return false, &UserError{Msg: fmt.Sprintf("Expected boolean for \"%s\"", key)}
	}
	return b, nil
}

// OptInt reads an optional z.number().int() with min/max and a zod default.
func (a *Args) OptInt(key string, def, min, max int) (int, error) {
	r, ok := a.raw[key]
	if !ok {
		return def, nil
	}
	if isExplicitNull(r) {
		return 0, &UserError{Msg: fmt.Sprintf("Expected number, received null: \"%s\"", key)}
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
	if v > max {
		return 0, &UserError{Msg: fmt.Sprintf("Number must be less than or equal to %d: \"%s\"", max, key)}
	}
	return v, nil
}

// OptEnum reads an optional z.enum(...) with a default.
func (a *Args) OptEnum(key, def string, values ...string) (string, error) {
	if !a.has(key) {
		return def, nil
	}
	s, err := a.ReqString(key, false)
	if err != nil {
		return "", err
	}
	for _, v := range values {
		if s == v {
			return s, nil
		}
	}
	return "", &UserError{Msg: fmt.Sprintf("Invalid enum value for \"%s\": %q", key, s)}
}
