package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TokenSource is the slice of *drive.Client the handlers need.
type TokenSource interface {
	AccessToken(ctx context.Context) (string, error)
	InvalidateAccessToken()
}

// Google is the handlers' shared REST transport. It deliberately does NOT use
// internal/drive's request(): relayed-tool failures in the TS gateway never
// fed the Drive health state machine, and each upstream tool formats its own
// error text from Google's error.message — so this client returns status and
// body and leaves formatting to the handler.
type Google struct {
	Tok     TokenSource
	HTTP    *http.Client
	Timeout time.Duration
}

// API bases are vars, not consts, so tests can point handlers at a fake
// server. Production code never mutates them.
var (
	DriveBase    = "https://www.googleapis.com/drive/v3"
	DocsBase     = "https://docs.googleapis.com/v1"
	SheetsBase   = "https://sheets.googleapis.com/v4"
	CalendarBase = "https://www.googleapis.com/calendar/v3"
)

// Do issues one authenticated request with the same retry-once-on-401
// contract as the enforce path. Non-2xx is NOT an error here: the handler
// reads Status and ErrMessage to build the upstream's message.
func (g *Google) Do(ctx context.Context, method, rawURL string, body any) (respBody []byte, status int, err error) {
	return g.do(ctx, method, rawURL, body, false)
}

func (g *Google) do(ctx context.Context, method, rawURL string, body any, retried bool) ([]byte, int, error) {
	tok, err := g.Tok.AccessToken(ctx)
	if err != nil {
		return nil, 0, err
	}
	if g.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, g.Timeout)
		defer cancel()
	}
	var rdr io.Reader
	if body != nil {
		raw, merr := json.Marshal(body)
		if merr != nil {
			return nil, 0, merr
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := g.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	res, err := hc.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request failed: %w", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if res.StatusCode == 401 && !retried {
		g.Tok.InvalidateAccessToken()
		return g.do(ctx, method, rawURL, body, true)
	}
	return raw, res.StatusCode, nil
}

// ErrMessage mirrors what the upstream tools actually splice into their
// "Failed to X: <message>" templates: gaxios's error.message, per
// GaxiosError.extractAPIErrorFromResponse:
//  1. a non-JSON body (gaxios leaves res.data a string) is the message, raw;
//  2. a JSON string body is the message;
//  3. {error: "<string>"} is the message;
//  4. {error: {message: "<string>"}} is the message — the error object's own
//     message wins even when errors[] is also present (gaxios spreads the
//     object over its computed message);
//  5. {error: {errors: [{message}]}} with no message key joins the messages
//     with "\n";
//  6. any other parsed JSON falls back to "Request failed with status code N".
//
// ("Unknown error" is the template's own fallback for an empty message.)
func ErrMessage(body []byte, status int) string {
	fallback := fmt.Sprintf("Request failed with status code %d", status)
	var data any
	if err := json.Unmarshal(bytes.TrimSpace(body), &data); err != nil {
		// Not JSON at all: gaxios keeps res.data a string. An empty message
		// hits the upstream template's `${error.message || 'Unknown error'}`.
		if len(body) == 0 {
			return "Unknown error"
		}
		return string(body)
	}
	if str, ok := data.(string); ok {
		return str
	}
	obj, ok := data.(map[string]any)
	if !ok {
		return fallback
	}
	e, ok := obj["error"]
	if !ok {
		return fallback
	}
	if str, ok := e.(string); ok {
		return str
	}
	if eo, ok := e.(map[string]any); ok {
		// gaxios 7.3.1: Object.assign({message: errors.join('\n') || message},
		// res.data.error) spreads the error object's fields LAST, so the
		// object's own "message" key wins whenever present; the errors[] join
		// applies only when the object carries no message of its own.
		if raw, present := eo["message"]; present {
			if msg, ok := raw.(string); ok {
				if msg == "" {
					return "Unknown error" // the upstream template's || fallback
				}
				return msg
			}
		}
		if errs, ok := eo["errors"].([]any); ok {
			var parts []string
			for _, item := range errs {
				if im, ok := item.(map[string]any); ok {
					if m, ok := im["message"].(string); ok {
						parts = append(parts, m)
					}
				}
			}
			if j := strings.Join(parts, "\n"); j != "" {
				return j
			}
		}
	}
	return fallback
}

// Query builds a query string; values are URL-encoded exactly once.
func Query(pairs ...string) string {
	if len(pairs)%2 != 0 {
		panic("Query: odd pairs")
	}
	v := url.Values{}
	for i := 0; i < len(pairs); i += 2 {
		v.Set(pairs[i], pairs[i+1])
	}
	return v.Encode()
}

// JoinURL appends an encoded path segment (file IDs, event IDs).
func JoinURL(base, segment string) string {
	return strings.TrimRight(base, "/") + "/" + url.PathEscape(segment)
}
