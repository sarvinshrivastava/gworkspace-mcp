// Broken-only pushes to one shared ntfy topic. The topic is the credential on
// public ntfy.sh: it is read from the vault, never logged, and every push body
// is a fixed template built by drive.Health.
// Port of src/notify.ts.
package notify

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/drive"
)

var topicRE = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

var loopbackHosts = map[string]bool{"localhost": true, "127.0.0.1": true, "[::1]": true}

// Notifier returns true when ntfy answered 2xx. Never errors out the caller.
type Notifier func(notice drive.Notice) bool

func asciiTitle(title string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return '?'
		}
		return r
	}, title)
}

// NewNotifier validates topic and base exactly as the TS notifierFrom, or
// returns nil (alerts log-only).
func NewNotifier(rawTopic, base string, timeout time.Duration) Notifier {
	topic := strings.TrimSpace(rawTopic)
	if topic == "" {
		fmt.Println("[alert] ntfy not configured — alerts are log-only")
		return nil
	}
	if !topicRE.MatchString(topic) {
		fmt.Println("[alert] ntfy disabled: invalid topic")
		return nil
	}
	u, err := url.Parse(base)
	if err != nil || u.RawQuery != "" || u.Fragment != "" || u.User != nil ||
		(u.Scheme != "https" && !(u.Scheme == "http" && loopbackHosts[u.Hostname()])) {
		fmt.Println("[alert] ntfy disabled: invalid base")
		return nil
	}
	target := strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/") + "/" + topic
	client := &http.Client{
		Timeout: timeout,
		// The https check on the base must not be undoable by a server's 30x.
		CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse },
	}
	return func(notice drive.Notice) (ok bool) {
		defer func() {
			if r := recover(); r != nil {
				ok = false
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, "POST", target, strings.NewReader(notice.Message))
		if err != nil {
			fmt.Println("[alert] ntfy push failed: request")
			return false
		}
		req.Header.Set("Title", asciiTitle(notice.Title))
		req.Header.Set("Priority", "high")
		req.Header.Set("Tags", "warning")
		req.Header.Set("Content-Type", "text/plain; charset=utf-8")
		res, err := client.Do(req)
		if err != nil {
			// The name only: the message can carry the URL, and the URL carries the topic.
			fmt.Printf("[alert] ntfy push failed: %T\n", err)
			return false
		}
		defer res.Body.Close()
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			fmt.Printf("[alert] ntfy push failed: HTTP %d\n", res.StatusCode)
			return false
		}
		return true
	}
}

// MakeBroadcaster: at most one delivered push per kind per gapMs, one send in
// flight per kind, and never more than one attempt per kind per minute. An
// undelivered push does not start the gap, so the next event of that kind
// retries it. No timers.
func MakeBroadcaster(notifier Notifier) func(drive.BrokenKind, drive.Notice) {
	const gapMs = 1_800_000
	now := func() int64 { return time.Now().UnixMilli() }
	var mu sync.Mutex
	lastDelivered := map[drive.BrokenKind]int64{}
	lastAttempt := map[drive.BrokenKind]int64{}
	inFlight := map[drive.BrokenKind]bool{}
	return func(kind drive.BrokenKind, notice drive.Notice) {
		if notifier == nil {
			return
		}
		mu.Lock()
		if inFlight[kind] {
			mu.Unlock()
			return
		}
		if now()-lastDelivered[kind] < gapMs {
			mu.Unlock()
			return
		}
		if now()-lastAttempt[kind] < 60_000 {
			mu.Unlock()
			return
		}
		inFlight[kind] = true
		lastAttempt[kind] = now()
		mu.Unlock()
		go func() {
			delivered := notifier(notice)
			mu.Lock()
			delete(inFlight, kind)
			if delivered {
				lastDelivered[kind] = now()
			}
			mu.Unlock()
		}()
	}
}
