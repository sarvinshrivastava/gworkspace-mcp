package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// fakeTokenSource is a static token source; it records invalidations so tests
// can assert the retry-once-on-401 contract.
type fakeTokenSource struct {
	mu           *sync.Mutex
	invalidated  int
	tokenCounter int
}

func newFakeTokenSource() *fakeTokenSource { return &fakeTokenSource{mu: &sync.Mutex{}} }

func (f *fakeTokenSource) AccessToken(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenCounter++
	return "fake-token", nil
}

func (f *fakeTokenSource) InvalidateAccessToken() {
	f.mu.Lock()
	f.invalidated++
	f.mu.Unlock()
}

// recordedRequest captures exactly what a handler sent to Google.
type recordedRequest struct {
	Method string
	Path   string
	Query  string
	Body   string
	Auth   string
}

// fakeGoogle is an httptest server that records requests and replays canned
// responses in order. Handlers under test get its URL swapped in for the
// real base via the Base override on Google.
type fakeGoogle struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	requests []recordedRequest
	handler  http.HandlerFunc
}

func newFakeGoogle(t *testing.T, handler http.HandlerFunc) *fakeGoogle {
	fg := &fakeGoogle{t: t, handler: handler}
	fg.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		if r.Body != nil {
			body, _ = readAll(r)
		}
		fg.mu.Lock()
		fg.requests = append(fg.requests, recordedRequest{
			Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
			Body: string(body), Auth: r.Header.Get("Authorization"),
		})
		fg.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(fg.server.Close)
	return fg
}

func readAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(r.Body)
}

func (fg *fakeGoogle) lastRequest() recordedRequest {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	if len(fg.requests) == 0 {
		fg.t.Fatal("no request recorded")
	}
	return fg.requests[len(fg.requests)-1]
}

// googleFor builds a *Google pointed at the fake; baseOverride replaces the
// hard-coded API base constants inside handlers via the test-only hook.
func (fg *fakeGoogle) googleFor(tok TokenSource) *Google {
	return &Google{Tok: tok, HTTP: fg.server.Client()}
}

// cannedJSON writes one JSON response.
func cannedJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func mustArgs(t *testing.T, s string) json.RawMessage {
	t.Helper()
	if s == "" {
		return nil
	}
	return json.RawMessage(s)
}
