package tools

import (
	"strings"
	"testing"
)

// Sheets handler regression fixes, pinned:
// 1. [[1],null] must reject (zod array-of-arrays), not decode as a nil row.
func TestCellValues_NullRowRejects(t *testing.T) {
	for _, key := range []string{"values", "initialData"} {
		a, err := ParseArgs(mustArgs(t, `{"`+key+`":[[1],null]}`), key)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if _, err := cellValues(a, key, true, 0, 0); err == nil {
			t.Fatalf("%s: null row accepted", key)
		} else if ue, ok := err.(*UserError); !ok || !strings.Contains(ue.Msg, "Expected array") {
			t.Fatalf("%s: err = %v", key, err)
		}
	}
	// A null CELL stays legal; only null ROWS reject.
	a, err := ParseArgs(mustArgs(t, `{"values":[[1,null,"x"]]}`), "values")
	if err != nil || a == nil {
		t.Fatalf("parse null-cell: a=%v err=%v", a, err)
	}
	rows, err := cellValues(a, "values", true, 0, 0)
	if err != nil || len(rows) != 1 || len(rows[0]) != 3 {
		t.Fatalf("null cell: rows=%v err=%v", rows, err)
	}
	// Non-array rows reject too.
	a, err = ParseArgs(mustArgs(t, `{"values":[[1],5]}`), "values")
	if err != nil || a == nil {
		t.Fatalf("parse scalar-row: a=%v err=%v", a, err)
	}
	if _, err := cellValues(a, "values", true, 0, 0); err == nil {
		t.Fatal("scalar row accepted")
	}
}

// 2. Explicit null for an optional scalar rejects, per zod .optional().
func TestArgs_ExplicitNullRejects(t *testing.T) {
	a, err := ParseArgs(mustArgs(t, `{"maxResults":null,"mimeType":null,"ownedByMe":null}`),
		"maxResults", "mimeType", "ownedByMe")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := a.OptInt("maxResults", 20, 1, 100); err == nil {
		t.Fatal("null int accepted")
	}
	if _, err := a.OptString("mimeType"); err == nil {
		t.Fatal("null string accepted")
	}
	if _, err := a.OptBool("ownedByMe"); err == nil {
		t.Fatal("null bool accepted")
	}
	// Absent keys still get defaults.
	a, _ = ParseArgs(mustArgs(t, `{}`), "maxResults")
	if v, err := a.OptInt("maxResults", 20, 1, 100); err != nil || v != 20 {
		t.Fatalf("absent default: %v %v", v, err)
	}
}

// 3. Stringify preserves HTML metacharacters AND U+2028/U+2029 raw, as
// JSON.stringify does.
func TestStringify_JSLiterals(t *testing.T) {
	out := Stringify(struct {
		S string `json:"s"`
	}{"<a>&\u2028\u2029"})
	if !strings.Contains(out, "\"s\": \"<a>&\u2028\u2029\"") {
		t.Fatalf("out = %q", out)
	}
	if strings.Contains(out, `<`) != true || strings.Contains(out, "&") != true {
		t.Fatalf("escaped: %q", out)
	}
	if strings.Contains(out, "\\u2028") || strings.Contains(out, "\\u2029") {
		t.Fatalf("line separators escaped: %q", out)
	}
}

// 4. ErrMessage mirrors gaxios extractAPIErrorFromResponse: a non-JSON body
// is the message raw; {error:"s"} and {error:{message:"s"}} extract; other
// parsed JSON falls back to "Request failed with status code N".
func TestErrMessage_GaxiosParity(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		status int
		want   string
	}{
		{"json error.message", `{"error":{"message":"bad range"}}`, 400, "bad range"},
		{"json error string", `{"error":"quota exceeded"}`, 429, "quota exceeded"},
		{"json no error field", `{"foo":1}`, 500, "Request failed with status code 500"},
		{"json error obj no message", `{"error":{"code":500}}`, 500, "Request failed with status code 500"},
		{"bare json string", `"oops"`, 400, "oops"},
		{"html body", `<html>Bad Gateway</html>`, 502, `<html>Bad Gateway</html>`},
		{"empty body", ``, 503, "Unknown error"},
	}
	for _, c := range cases {
		if got := ErrMessage([]byte(c.body), c.status); got != c.want {
			t.Fatalf("%s: ErrMessage = %q, want %q", c.name, got, c.want)
		}
	}
}
