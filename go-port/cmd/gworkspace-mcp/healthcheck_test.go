package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRunHealthcheck(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("path = %s", r.URL.Path)
		}
	}))
	defer ok.Close()
	_, port, _ := net.SplitHostPort(ok.Listener.Addr().String())
	var p int
	for _, c := range port {
		p = p*10 + int(c-'0')
	}
	if got := runHealthcheck(p); got != 0 {
		t.Errorf("2xx = %d, want 0", got)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer bad.Close()
	_, bp, _ := net.SplitHostPort(bad.Listener.Addr().String())
	p = 0
	for _, c := range bp {
		p = p*10 + int(c-'0')
	}
	if got := runHealthcheck(p); got != 1 {
		t.Errorf("503 = %d, want 1", got)
	}
	if got := runHealthcheck(1); got != 1 {
		t.Errorf("unreachable = %d, want 1", got)
	}
}

func TestHealthcheckExitFlag(t *testing.T) {
	if healthcheckExit() != nil {
		t.Errorf("no flag must return nil")
	}
}
