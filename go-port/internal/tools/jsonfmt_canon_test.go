package tools

import (
	"strings"
	"testing"
)

// Every expectation below was captured from node (JSON.stringify), not
// derived from the Go implementation.
func TestJSCanonicalNumber(t *testing.T) {
	cases := []struct{ in, want string }{
		{"1e2", "100"},
		{"11.50", "11.5"},
		{"0.00001", "0.00001"},
		{"1e-7", "1e-7"},
		{"1.5e21", "1.5e+21"},
		{"-0", "0"},
		{"295147905179352825856", "295147905179352830000"}, // 2^68: shortest round-trip
		{"123456789012345680000", "123456789012345680000"},
		{"0.1", "0.1"},
		{"0.333333333333333333333", "0.3333333333333333"},
		{"1e-6", "0.000001"},
		{"1e21", "1e+21"},
		{"2.5e-7", "2.5e-7"},
		{"42", "42"},
		{"-3.25", "-3.25"},
	}
	for _, c := range cases {
		if got := jsNumberCanon(c.in); got != c.want {
			t.Errorf("jsNumberCanon(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestJSStringifyRawCanonicalizesNumbers(t *testing.T) {
	raw := `{"a":1e2,"b":11.50,"c":[1e-7,-0,2.5e-7],"s":"x"}`
	got := jsStringifyRaw([]byte(raw), 0)
	for _, want := range []string{`"a": 100`, `"b": 11.5`, `"c": [
    1e-7,
    0,
    2.5e-7
  ]`, `"s": "x"`} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestErrMessagePrecedence(t *testing.T) {
	// gaxios spreads the error object over its computed message: an own
	// message key WINS even when errors[] is present (verified against Node).
	body := []byte(`{"error":{"message":"top","errors":[{"message":"first"},{"message":"second"}]}}`)
	if got := ErrMessage(body, 400); got != "top" {
		t.Errorf("own message must win over errors[], got %q", got)
	}
	// No message key: the errors[] join applies.
	body = []byte(`{"error":{"errors":[{"message":"first"},{"message":"second"}]}}`)
	if got := ErrMessage(body, 400); got != "first\nsecond" {
		t.Errorf("errors-only should join, got %q", got)
	}
	body = []byte(`{"error":{"errors":[{"message":"only"}]}}`)
	if got := ErrMessage(body, 400); got != "only" {
		t.Errorf("single join, got %q", got)
	}
	// Present-but-empty message: gaxios spreads "" and the upstream template
	// turns it into "Unknown error".
	body = []byte(`{"error":{"message":"","errors":[{"message":"x"}]}}`)
	if got := ErrMessage(body, 400); got != "Unknown error" {
		t.Errorf("empty message must hit the template fallback, got %q", got)
	}
	// Empty join and no message: status fallback.
	body = []byte(`{"error":{"errors":[]}}`)
	if got := ErrMessage(body, 400); got != "Request failed with status code 400" {
		t.Errorf("bare errors array should hit fallback, got %q", got)
	}
}
