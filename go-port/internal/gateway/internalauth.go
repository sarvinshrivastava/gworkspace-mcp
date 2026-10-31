// The guard for /internal/* — routes that only pipeline-runner calls, over
// the `shared` Docker network, with its own bearer (GWORKSPACE_PLR_TOKEN).
// Port of src/internal-auth.ts.
package gateway

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/remote"
)

const (
	windowMs     = 60_000
	maxPerWindow = 10
)

// UsablePlrToken: the vault's GWORKSPACE_PLR_TOKEN, or "" when unusable.
// Anything under 32 characters after trimming counts as not configured.
func UsablePlrToken(raw string) string {
	token := strings.TrimSpace(raw)
	if len(token) >= 32 {
		return token
	}
	return ""
}

type plrBucket struct {
	start time.Time
	count int
}

// internalGuard reproduces the TS guard in order:
//  1. exact lowercase /internal/ prefix, else 404
//  2. a proxy header (X-Forwarded-For / X-Real-IP), else 404
//  3. 10 requests per source per minute, else 429 (before the bearer)
//  4. no token configured → 503; never a match against ""
//  5. constant-time bearer compare, else 401 (logged once per source/min)
type internalGuard struct {
	token    func() string
	onReject func(source string)
	now      func() time.Time
	mu       sync.Mutex
	buckets  map[string]*plrBucket
	lastLog  map[string]time.Time
}

func newInternalGuard(token func() string, onReject func(string)) *internalGuard {
	return &internalGuard{
		token:    token,
		onReject: onReject,
		now:      time.Now,
		buckets:  map[string]*plrBucket{},
		lastLog:  map[string]time.Time{},
	}
}

func (g *internalGuard) sweep(at time.Time) {
	for source, b := range g.buckets {
		if at.Sub(b.start) >= windowMs*time.Millisecond {
			delete(g.buckets, source)
		}
	}
	for source, logged := range g.lastLog {
		if at.Sub(logged) >= windowMs*time.Millisecond {
			delete(g.lastLog, source)
		}
	}
}

func (g *internalGuard) ServeHTTP(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	// TS: req.headers[...] !== undefined — presence, not a non-empty value.
	// An empty X-Forwarded-For still proves a proxy hop, so it still 404s.
	_, hasXFF := r.Header["X-Forwarded-For"]
	_, hasXRI := r.Header["X-Real-Ip"]
	proxied := hasXFF || hasXRI
	// Go's ServeMux already matched the literal "/internal/" prefix
	// case-sensitively; the proxied check mirrors the TS layer 2.
	if proxied {
		w.WriteHeader(404)
		return
	}

	source := remote.Host(r.RemoteAddr)
	at := g.now()
	g.mu.Lock()
	g.sweep(at)
	bucket, ok := g.buckets[source]
	if !ok {
		g.buckets[source] = &plrBucket{start: at, count: 1}
	} else if bucket.count >= maxPerWindow {
		retry := int((bucket.start.Add(windowMs*time.Millisecond).Sub(at) + time.Second - 1) / time.Second)
		g.mu.Unlock()
		w.Header().Set("Retry-After", fmt.Sprintf("%d", retry))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":"rate_limited"}`)
		return
	} else {
		bucket.count++
	}
	g.mu.Unlock()

	expected := g.token()
	if expected == "" {
		writeJSON(w, 503, `{"error":"not_configured"}`)
		return
	}
	provided := bearerOf(r)
	sumProvided := sha256.Sum256([]byte(provided))
	sumExpected := sha256.Sum256([]byte(expected))
	if subtle.ConstantTimeCompare(sumProvided[:], sumExpected[:]) != 1 {
		g.mu.Lock()
		if _, logged := g.lastLog[source]; !logged {
			g.lastLog[source] = at
			// %q: the decoded path is caller text. A newline in it would forge
			// log lines, which the deploy greps and a failed run prints.
			fmt.Printf("[internal] rejected %s %q from %s\n", r.Method, r.URL.Path, orUnknown(source))
		}
		g.mu.Unlock()
		if g.onReject != nil {
			g.onReject(source)
		}
		writeJSON(w, 401, `{"error":"unauthorized"}`)
		return
	}
	next(w, r)
}

// bearerRe is the TS /^Bearer\s+/i with the JS \s class spelled out
// (Go's \s lacks \v and the Unicode spaces; the TS accept surface is pinned).
var bearerRe = regexp.MustCompile("(?i)^Bearer[\\t\\n\\v\\f\\r \\x{a0}\\x{1680}\\x{2000}-\\x{200a}\\x{2028}\\x{2029}\\x{202f}\\x{205f}\\x{3000}\\x{feff}]+")

func bearerOf(r *http.Request) string {
	return bearerRe.ReplaceAllString(r.Header.Get("Authorization"), "")
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprint(w, body)
}
