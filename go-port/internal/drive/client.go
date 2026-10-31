// Package drive: a minimal Drive v3 + Calendar client for the gateway's own
// lookups, and the error taxonomy the pipeline depends on.
// Port of src/drive/client.ts.
package drive

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const FolderMime = "application/vnd.google-apps.folder"

const (
	tokenURL    = "https://oauth2.googleapis.com/token"
	driveURL    = "https://www.googleapis.com/drive/v3/files"
	calendarURL = "https://www.googleapis.com/calendar/v3/calendars"
)

var rateLimitReasons = map[string]bool{"rateLimitExceeded": true, "userRateLimitExceeded": true}

// ErrorSource: which call an error came from, so a Calendar rejection is
// never read as a dead Drive credential.
type ErrorSource string

const (
	SourceOAuth    ErrorSource = "oauth"
	SourceDrive    ErrorSource = "drive"
	SourceCalendar ErrorSource = "calendar"
)

// Error separates "Google could not answer right now" (Retryable: rate
// limits, 5xx, network) from "Google answered no". Rejection is set when
// Google answered with a standing "no" that retrying cannot fix.
type Error struct {
	Status    int
	Retryable bool
	Msg       string
	Rejection string
	Source    ErrorSource
}

func (e *Error) Error() string { return e.Msg }

// RejectionDetail: an OAuth category stands alone; an API one names its service.
func RejectionDetail(e *Error) string {
	if e.Source == SourceOAuth {
		return e.Rejection
	}
	return string(e.Source) + " " + e.Rejection
}

// CategoryOf is the one place a failure's short category is derived. Never
// the message itself, so a hostile or secret-bearing message cannot reach a push.
func CategoryOf(err error) string {
	var de *Error
	if As(err, &de) {
		if de.Rejection != "" {
			return RejectionDetail(de)
		}
		if de.Status >= 400 {
			return fmt.Sprintf("HTTP %d", de.Status)
		}
	}
	msg := err.Error()
	if isTimeout(err) || containsFold(msg, "abort") || containsFold(msg, "timeout") {
		return "timeout"
	}
	if strings.Contains(msg, "not ready") {
		return "unavailable"
	}
	return "network"
}

func As(err error, target **Error) bool {
	for err != nil {
		if de, ok := err.(*Error); ok {
			*target = de
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func isTimeout(err error) bool {
	type timeouter interface{ Timeout() bool }
	for err != nil {
		if t, ok := err.(timeouter); ok && t.Timeout() {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

var oauthCodeRE = regexp.MustCompile(`^[a-z_]{1,40}$`)

// oauthCategory reads only the OAuth `error` code of a failed refresh.
// error_description is never read: it can echo the request.
func oauthCategory(status int, body []byte) string {
	var parsed struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil && oauthCodeRE.MatchString(parsed.Error) {
		return parsed.Error
	}
	return fmt.Sprintf("HTTP %d", status)
}

// gaxiosMessage mirrors gaxios 7.3.1 extractAPIErrorFromResponse (data.error
// string wins; error object uses its message or errors[].message joined "\n";
// a non-JSON body is the message verbatim; otherwise the status-code default),
// plus google-auth-library's 403/404 "Could not refresh access token: " prefix
// in OAuth2Client.getRequestMetadataAsync. error_description is never read.
func gaxiosMessage(status int, body []byte) string {
	msg := fmt.Sprintf("Request failed with status code %d", status)
	var parsed any
	if err := json.Unmarshal(body, &parsed); err == nil {
		if m, ok := parsed.(map[string]any); ok {
			switch e := m["error"].(type) {
			case string:
				if e != "" {
					msg = e
				}
			case map[string]any:
				// gaxios spreads the error object's fields over its computed
				// message: an own "message" key wins; the errors[] join applies
				// only when no message key is present.
				if raw, present := e["message"]; present {
					if em, ok := raw.(string); ok {
						if em == "" {
							msg = ""
						} else {
							msg = em
						}
						break
					}
				}
				if errs, ok := e["errors"].([]any); ok {
					var parts []string
					for _, item := range errs {
						if im, ok := item.(map[string]any); ok {
							if em, ok := im["message"].(string); ok {
								parts = append(parts, em)
							}
						}
					}
					if joined := strings.Join(parts, "\n"); joined != "" {
						msg = joined
					}
				}
			}
		}
	} else if s := strings.TrimSpace(string(body)); s != "" {
		msg = s
	}
	if status == 403 || status == 404 {
		return "Could not refresh access token: " + msg
	}
	if msg == "" {
		return "Unknown error" // the upstream template's || fallback
	}
	return msg
}

var authScopesRE = regexp.MustCompile(`authentication scopes`)

// serviceRejection classifies Google's service-level 403s: the API is off, or
// the grant lacks the scope. Not a per-file denial.
func serviceRejection(reasons []string, message string) string {
	for _, r := range reasons {
		if r == "accessNotConfigured" || r == "SERVICE_DISABLED" {
			return "accessNotConfigured"
		}
	}
	for _, r := range reasons {
		if r == "ACCESS_TOKEN_SCOPE_INSUFFICIENT" {
			return "insufficientPermissions"
		}
	}
	if authScopesRE.MatchString(message) {
		return "insufficientPermissions"
	}
	return ""
}

type Creds struct {
	ClientID     string
	ClientSecret string
	RefreshToken string
}

type File struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	MimeType string   `json:"mimeType"`
	Parents  []string `json:"parents"`
	Trashed  bool     `json:"trashed"`
}

type ListPage struct {
	Files            []map[string]any `json:"files"`
	NextPageToken    string           `json:"nextPageToken,omitempty"`
	IncompleteSearch bool             `json:"incompleteSearch,omitempty"`
}

type Client struct {
	creds   Creds
	http    *http.Client
	timeout time.Duration
	now     func() int64
	onOk    func(startedAt int64)

	mu          sync.Mutex
	accessToken string
	expiresAt   int64
	refreshing  chan struct{} // non-nil while a refresh is in flight
	refreshErr  error
}

func NewClient(creds Creds, timeout time.Duration, onOk func(startedAt int64)) *Client {
	return &Client{
		creds:   creds,
		http:    &http.Client{},
		timeout: timeout,
		now:     func() int64 { return time.Now().UnixMilli() },
		onOk:    onOk,
	}
}

// token returns a valid access token; exactly one refresh is in flight at a
// time and every concurrent caller awaits it.
func (c *Client) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.accessToken != "" && c.expiresAt > c.now() {
		tok := c.accessToken
		c.mu.Unlock()
		return tok, nil
	}
	if c.refreshing != nil {
		wait := c.refreshing
		c.mu.Unlock()
		select {
		case <-wait:
			c.mu.Lock()
			tok, err := c.accessToken, c.refreshErr
			c.mu.Unlock()
			if err != nil {
				return "", err
			}
			return tok, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	c.refreshing = make(chan struct{})
	c.mu.Unlock()

	token, err := c.refresh(ctx)

	c.mu.Lock()
	if err == nil {
		c.accessToken = token
	}
	c.refreshErr = err
	close(c.refreshing)
	c.refreshing = nil
	c.mu.Unlock()
	return token, err
}

func (c *Client) refresh(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {c.creds.ClientID},
		"client_secret": {c.creds.ClientSecret},
		"refresh_token": {c.creds.RefreshToken},
	}
	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := c.http.Do(req)
	if err != nil {
		return "", &Error{0, true, "token refresh failed: " + err.Error(), "", SourceOAuth}
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		transient := res.StatusCode >= 500 || res.StatusCode == 429 || res.StatusCode == 408
		rejection := ""
		if !transient {
			rejection = oauthCategory(res.StatusCode, body)
		}
		return "", &Error{res.StatusCode, transient, gaxiosMessage(res.StatusCode, body), rejection, SourceOAuth}
	}
	var parsed struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.AccessToken == "" {
		return "", &Error{res.StatusCode, true, "token refresh returned no token", "", SourceOAuth}
	}
	expiresIn := parsed.ExpiresIn
	if expiresIn == 0 {
		expiresIn = 3600
	}
	c.mu.Lock()
	c.expiresAt = c.now() + (expiresIn-60)*1000
	c.mu.Unlock()
	return parsed.AccessToken, nil
}

func (c *Client) request(ctx context.Context, rawURL string, source ErrorSource, retried bool, startedAt int64) ([]byte, int, error) {
	tok, err := c.token(ctx)
	if err != nil {
		return nil, 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := c.http.Do(req)
	if err != nil {
		return nil, 0, &Error{0, true, "Drive unreachable: " + err.Error(), "", source}
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 16<<20))

	if res.StatusCode == 401 {
		c.mu.Lock()
		c.accessToken = ""
		c.mu.Unlock()
		if !retried {
			return c.request(ctx, rawURL, source, true, startedAt)
		}
		return nil, 401, &Error{401, true, "Drive rejected a freshly refreshed token", "", source}
	}
	answered := func() {
		// Only Drive's own answers say the credential works; Calendar's never do.
		if source == SourceDrive && c.onOk != nil {
			c.onOk(startedAt)
		}
	}
	if (res.StatusCode >= 200 && res.StatusCode < 300) || res.StatusCode == 404 {
		answered()
		return body, res.StatusCode, nil
	}

	var reasons []string
	var message string
	var parsed struct {
		Error struct {
			Message string `json:"message"`
			Errors  []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
			Details []struct {
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil {
		if len(parsed.Error.Errors) > 0 && parsed.Error.Errors[0].Reason != "" {
			reasons = append(reasons, parsed.Error.Errors[0].Reason)
		}
		for _, d := range parsed.Error.Details {
			if d.Reason != "" {
				reasons = append(reasons, d.Reason)
			}
		}
		message = parsed.Error.Message
	}
	reason := ""
	if len(reasons) > 0 {
		reason = reasons[0]
	}
	if res.StatusCode == 403 {
		if rejection := serviceRejection(reasons, message); rejection != "" {
			return nil, 403, &Error{403, false, fmt.Sprintf("Drive HTTP 403 (%s)", rejection), rejection, source}
		}
	}
	retryable := res.StatusCode == 429 || res.StatusCode >= 500 ||
		(res.StatusCode == 403 && (rateLimitReasons[reasons0(reasons)] || anyRateLimit(reasons)))
	if !retryable {
		answered()
	}
	msg := fmt.Sprintf("Drive HTTP %d", res.StatusCode)
	if reason != "" {
		msg += " (" + reason + ")"
	}
	return nil, res.StatusCode, &Error{res.StatusCode, retryable, msg, "", source}
}

func reasons0(r []string) string {
	if len(r) == 0 {
		return ""
	}
	return r[0]
}

func anyRateLimit(reasons []string) bool {
	for _, r := range reasons {
		if rateLimitReasons[r] {
			return true
		}
	}
	return false
}

// GetFile: nil when Drive says 404.
func (c *Client) GetFile(ctx context.Context, id string) (*File, error) {
	params := url.Values{
		"fields":            {"id,name,mimeType,parents,trashed"},
		"supportsAllDrives": {"true"},
	}
	body, status, err := c.request(ctx, driveURL+"/"+url.PathEscape(id)+"?"+params.Encode(), SourceDrive, false, c.now())
	if err != nil {
		return nil, err
	}
	if status == 404 {
		return nil, nil
	}
	var f File
	if err := json.Unmarshal(body, &f); err != nil {
		return nil, err
	}
	if f.ID == "" {
		f.ID = id
	}
	return &f, nil
}

// EventGuests: the guests of one calendar event, without this account itself.
// Nil when the event does not exist.
func (c *Client) EventGuests(ctx context.Context, calendarID, eventID string) ([]string, error) {
	params := url.Values{"fields": {"attendees(email,self)"}}
	body, status, err := c.request(ctx,
		calendarURL+"/"+url.PathEscape(calendarID)+"/events/"+url.PathEscape(eventID)+"?"+params.Encode(),
		SourceCalendar, false, c.now())
	if err != nil {
		return nil, err
	}
	if status == 404 {
		return nil, nil
	}
	var parsed struct {
		Attendees []struct {
			Email string `json:"email"`
			Self  bool   `json:"self"`
		} `json:"attendees"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	var out []string
	for _, a := range parsed.Attendees {
		if !a.Self {
			out = append(out, a.Email)
		}
	}
	return out, nil
}

type ListParams struct {
	Q         string
	Fields    string
	PageSize  int
	OrderBy   string
	PageToken string
}

func (c *Client) ListFiles(ctx context.Context, p ListParams) (*ListPage, error) {
	params := url.Values{
		"q":                         {p.Q},
		"fields":                    {p.Fields},
		"pageSize":                  {fmt.Sprint(p.PageSize)},
		"supportsAllDrives":         {"true"},
		"includeItemsFromAllDrives": {"true"},
		"corpora":                   {"allDrives"},
	}
	if p.OrderBy != "" {
		params.Set("orderBy", p.OrderBy)
	}
	if p.PageToken != "" {
		params.Set("pageToken", p.PageToken)
	}
	body, status, err := c.request(ctx, driveURL+"?"+params.Encode(), SourceDrive, false, c.now())
	if err != nil {
		return nil, err
	}
	if status == 404 {
		return nil, &Error{404, false, "Drive HTTP 404", "", SourceDrive}
	}
	var page ListPage
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, err
	}
	if page.Files == nil {
		page.Files = []map[string]any{}
	}
	return &page, nil
}

// AccessToken exposes the shared OAuth token machinery to the tool handlers,
// which speak to Docs/Sheets/Calendar endpoints the enforce path never
// touches. One process-wide token cache, same single-flight refresh.
func (c *Client) AccessToken(ctx context.Context) (string, error) { return c.token(ctx) }

// InvalidateAccessToken drops the cached token after a 401 so the next call
// refreshes — the same retry-once contract request() uses.
func (c *Client) InvalidateAccessToken() {
	c.mu.Lock()
	c.accessToken = ""
	c.mu.Unlock()
}
