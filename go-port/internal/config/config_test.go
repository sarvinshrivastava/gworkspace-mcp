package config

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseVaultBody(t *testing.T) {
	if _, err := parseVaultBody("not json", "Root/k"); err == nil ||
		!strings.Contains(err.Error(), "unparseable JSON") {
		t.Errorf("unparseable: %v", err)
	}
	if _, err := parseVaultBody(`{"value":""}`, "Root/k"); err == nil {
		t.Errorf("empty value must fail")
	}
	if _, err := parseVaultBody(`{"other":1}`, "Root/k"); err == nil {
		t.Errorf("missing value must fail")
	}
	v, err := parseVaultBody(`{"value":"sekret"}`, "Root/k")
	if err != nil || v != "sekret" {
		t.Errorf("ok case: %q %v", v, err)
	}
}

func vaultServer(t *testing.T, status int, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing bearer on vault request")
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
}

func testConfig(url string) *Config {
	c := &Config{HTTPTimeout: 2 * time.Second}
	c.Vault.URL = url
	c.Vault.Folder = "Root"
	c.Vault.Token = "tok"
	return c
}

func TestVaultFetchOK(t *testing.T) {
	s := vaultServer(t, 200, `{"value":"v1"}`)
	defer s.Close()
	v, err := testConfig(s.URL).VaultFetch(context.Background(), "k", false)
	if err != nil || v != "v1" {
		t.Errorf("got %q %v", v, err)
	}
}

func TestVaultFetch404Optional(t *testing.T) {
	s := vaultServer(t, 404, "")
	defer s.Close()
	v, err := testConfig(s.URL).VaultFetch(context.Background(), "k", true)
	if err != nil || v != "" {
		t.Errorf("optional 404 = %q %v, want empty nil", v, err)
	}
}

func TestVaultFetch401FatalNoRetry(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(401)
	}))
	defer s.Close()
	_, err := testConfig(s.URL).VaultFetch(context.Background(), "k", false)
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Errorf("401: %v", err)
	}
	if calls != 1 {
		t.Errorf("401 must never retry, got %d calls", calls)
	}
}

func TestVaultFetchNoToken(t *testing.T) {
	c := testConfig("http://127.0.0.1:1")
	c.Vault.Token = ""
	if _, err := c.VaultFetch(context.Background(), "k", false); err == nil ||
		!strings.Contains(err.Error(), "SECRETS_MANAGER_TOKEN") {
		t.Errorf("empty token: %v", err)
	}
}

func TestOneOfPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Errorf("oneOf with a bad value must panic")
		}
	}()
	t.Setenv("GW_TEST_ONEOF", "bogus")
	oneOf("GW_TEST_ONEOF", []string{"a", "b"}, "a")
}
