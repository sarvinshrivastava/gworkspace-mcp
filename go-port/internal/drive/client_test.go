package drive

import (
	"strings"
	"testing"
)

func TestOauthCategory(t *testing.T) {
	// Only the error code is read; error_description is never consulted.
	if got := oauthCategory(400, []byte(`{"error":"invalid_grant","error_description":"Token has been revoked"}`)); got != "invalid_grant" {
		t.Errorf("got %q", got)
	}
	if got := oauthCategory(401, []byte(`{"error":"invalid_client"}`)); got != "invalid_client" {
		t.Errorf("got %q", got)
	}
	// A description-shaped injection in the error field is not a code.
	if got := oauthCategory(400, []byte(`{"error":"not a code!"}`)); got != "HTTP 400" {
		t.Errorf("non-code error = %q, want HTTP 400", got)
	}
	if got := oauthCategory(500, []byte("garbage")); got != "HTTP 500" {
		t.Errorf("garbage = %q", got)
	}
}

func TestGaxiosMessage(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{401, `{"error":"invalid_client","error_description":"x"}`, "invalid_client"},
		{400, `{"error":{"message":"bad request"}}`, "bad request"},
		// gaxios spreads the error object over its computed message: an own
		// message key wins even with errors[] present (verified against Node).
		{400, `{"error":{"message":"top","errors":[{"message":"a"},{"message":"b"}]}}`, "top"},
		{400, `{"error":{"errors":[{"message":"a"},{"message":"b"}]}}`, "a\nb"},
		{400, `{"error":{"message":"","errors":[{"message":"a"}]}}`, "Unknown error"},
		{403, `{"error":{"message":"m","errors":[{"message":"a"}]}}`, "Could not refresh access token: m"},
		{403, `{"error":"accessNotConfigured"}`, "Could not refresh access token: accessNotConfigured"},
		{404, `{"error":"deleted_client"}`, "Could not refresh access token: deleted_client"},
		{401, `{"error":{"message":""}}`, "Unknown error"},
		{500, "upstream melted", "upstream melted"},
		{401, "", "Request failed with status code 401"},
		{401, `{"unexpected":1}`, "Request failed with status code 401"},
	}
	for _, c := range cases {
		if got := gaxiosMessage(c.status, []byte(c.body)); got != c.want {
			t.Errorf("gaxiosMessage(%d, %s) = %q, want %q", c.status, c.body, got, c.want)
		}
	}
}

func TestSafeCategory(t *testing.T) {
	if got := safeCategory("invalid_grant"); got != "invalid_grant" {
		t.Errorf("got %q", got)
	}
	if got := safeCategory(strings.Repeat("a", 41)); got != "unknown" {
		t.Errorf("over-long = %q", got)
	}
	if got := safeCategory("bad;DROP"); got != "unknown" {
		t.Errorf("unsafe = %q", got)
	}
}
