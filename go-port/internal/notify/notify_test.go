package notify

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/drive"
)

func TestNewNotifierValidation(t *testing.T) {
	ok := "goodtopic1"
	for _, tc := range []struct {
		topic, base string
		wantNil     bool
	}{
		{"", "https://ntfy.sh", true},
		{"short", "https://ntfy.sh", true},
		{"has space!", "https://ntfy.sh", true},
		{ok, "https://ntfy.sh", false},
		{ok, "https://ntfy.sh/", false},
		{ok, "https://ntfy.sh/path?x=1", true}, // query
		{ok, "https://user@ntfy.sh", true},     // userinfo
		{ok, "http://ntfy.sh", true},           // plain http off-loopback
		{ok, "http://localhost:8080", false},   // loopback http allowed
		{ok, "http://127.0.0.1:8080", false},   //
		{ok, "ftp://ntfy.sh", true},            //
	} {
		if n := NewNotifier(tc.topic, tc.base, time.Second); (n == nil) != tc.wantNil {
			t.Errorf("NewNotifier(%q, %q) nil=%v, want nil=%v", tc.topic, tc.base, n == nil, tc.wantNil)
		}
	}
}

func TestNotifierPushShape(t *testing.T) {
	var seen struct {
		title, priority, tags, body string
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.title = r.Header.Get("Title")
		seen.priority = r.Header.Get("Priority")
		seen.tags = r.Header.Get("Tags")
		b := make([]byte, 64)
		n, _ := r.Body.Read(b)
		seen.body = string(b[:n])
		if !strings.HasSuffix(r.URL.Path, "/testtopic1") {
			t.Errorf("path = %s", r.URL.Path)
		}
	}))
	defer s.Close()
	n := NewNotifier("testtopic1", s.URL, 2*time.Second)
	if n == nil {
		t.Fatal("loopback http notifier must build")
	}
	if !n(drive.Notice{Title: "tité\nx", Message: "hello body"}) {
		t.Fatal("2xx must report true")
	}
	if seen.title != "tit??x" { // non-ASCII + control chars map to ?
		t.Errorf("title = %q", seen.title)
	}
	if seen.priority != "high" || seen.tags != "warning" || seen.body != "hello body" {
		t.Errorf("headers/body = %+v", seen)
	}
}

func TestNotifierNon2xx(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer s.Close()
	n := NewNotifier("testtopic1", s.URL, 2*time.Second)
	if n(drive.Notice{Title: "t", Message: "m"}) {
		t.Fatal("500 must report false")
	}
}

func TestBroadcasterGapAndRetry(t *testing.T) {
	var delivered int64
	var release = make(chan struct{})
	notifier := func(drive.Notice) bool {
		<-release // hold the first send in flight
		atomic.AddInt64(&delivered, 1)
		return true
	}
	b := MakeBroadcaster(notifier)
	notice := drive.Notice{Title: "t", Message: "m"}
	b(drive.BrokenRejected, notice)
	b(drive.BrokenRejected, notice) // in-flight: dropped
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt64(&delivered) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if atomic.LoadInt64(&delivered) != 1 {
		t.Fatalf("in-flight drop failed: %d sends", delivered)
	}
	b(drive.BrokenRejected, notice) // delivered seconds ago: inside the 30 min gap
	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt64(&delivered) != 1 {
		t.Fatalf("gap suppression failed: %d sends", delivered)
	}
	// A different kind is not suppressed.
	b(drive.BrokenProbe, notice)
	deadline = time.Now().Add(2 * time.Second)
	for atomic.LoadInt64(&delivered) == 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if atomic.LoadInt64(&delivered) != 2 {
		t.Fatalf("per-kind isolation failed: %d sends", delivered)
	}
	// nil notifier: never panics, never sends.
	MakeBroadcaster(nil)(drive.BrokenRejected, notice)
}
