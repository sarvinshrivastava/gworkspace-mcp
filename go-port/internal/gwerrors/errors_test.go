package gwerrors

import (
	"errors"
	"testing"
)

func TestErrorMessage(t *testing.T) {
	if got := ErrorMessage(nil); got != "" {
		t.Errorf("nil = %q, want empty", got)
	}
	if got := ErrorMessage(errors.New("boom")); got != "boom" {
		t.Errorf("got %q", got)
	}
}

func TestRedactDetailSecrets(t *testing.T) {
	for _, s := range []string{
		`{"refresh_token":"abc"}`, "client_secret=x", "ACCESS_TOKEN leaked",
		"Bearer abc.def", `"token"`, // credential-shaped text never logs
	} {
		if got := RedactDetail(s, 300); got != "[redacted]" {
			t.Errorf("RedactDetail(%q) = %q, want [redacted]", s, got)
		}
	}
}

func TestRedactDetailControlChars(t *testing.T) {
	if got := RedactDetail("a\nb\tc\x00d\x7fe", 0); got != "a b c d e" {
		t.Errorf("control chars not flattened: %q", got)
	}
}

func TestRedactDetailUTF16Cap(t *testing.T) {
	// TS .slice(0, n) counts UTF-16 units: an astral rune is 2.
	if got := RedactDetail("abcde", 3); got != "abc" {
		t.Errorf("basic cap: %q", got)
	}
	// 2 BMP + cap 3: the astral rune (2 units) does not fit.
	if got := RedactDetail("ab\U0001F600z", 3); got != "ab" {
		t.Errorf("astral cap: %q, want %q", got, "ab")
	}
	if got := RedactDetail("ab\U0001F600z", 4); got != "ab\U0001F600" {
		t.Errorf("astral fits at 4: %q", got)
	}
}
