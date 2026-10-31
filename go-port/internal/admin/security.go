// The dashboard's gates: who may connect, the password, the lockout, sessions.
// Port of src/admin/security.ts.
package admin

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/scrypt"
)

// --- who may connect -------------------------------------------------------

// NetInfo abstracts interface and route-table reads for tests.
type NetInfo struct {
	Interfaces func() map[string][]net.Addr // interface name → addrs
	RouteText  func() string                // /proc/net/route, "" where none
}

func DefaultNet() NetInfo {
	return NetInfo{
		Interfaces: func() map[string][]net.Addr {
			out := map[string][]net.Addr{}
			ifaces, err := net.Interfaces()
			if err != nil {
				return out
			}
			for _, iface := range ifaces {
				addrs, err := iface.Addrs()
				if err != nil {
					continue
				}
				out[iface.Name] = addrs
			}
			return out
		},
		RouteText: func() string {
			raw, err := os.ReadFile("/proc/net/route")
			if err != nil {
				return ""
			}
			return string(raw)
		},
	}
}

var hex8RE = regexp.MustCompile(`^[0-9A-Fa-f]{8}$`)

// GatewaysFromRoute: little-endian hex in the third column.
func GatewaysFromRoute(text string) []string {
	seen := map[string]bool{}
	var out []string
	for i, line := range strings.Split(text, "\n") {
		if i == 0 {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 || !hex8RE.MatchString(fields[2]) {
			continue
		}
		hexv := fields[2]
		allZero := true
		b := make([]byte, 4)
		for j := 0; j < 4; j++ {
			v, _ := strconv.ParseUint(hexv[j*2:j*2+2], 16, 8)
			if v != 0 {
				allZero = false
			}
			b[3-j] = byte(v) // little-endian → dotted order
		}
		if allZero {
			continue
		}
		ip := net.IP(b).String()
		if !seen[ip] {
			seen[ip] = true
			out = append(out, ip)
		}
	}
	return out
}

// FirstHosts: the first host of each attached IPv4 network — Docker's default
// bridge gateway, which is where host traffic arrives from.
func FirstHosts(ifaces map[string][]net.Addr) []string {
	seen := map[string]bool{}
	var out []string
	for _, addrs := range ifaces {
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() {
				continue
			}
			network := ip4.Mask(ipnet.Mask)
			// TS: (network + 1) >>> 0 — a 32-bit increment, which matters when
			// the network address ends in .255 (e.g. a /32 or an unusual mask):
			// last-octet++ would wrap within the octet instead of carrying.
			first := make(net.IP, 4)
			binary.BigEndian.PutUint32(first, binary.BigEndian.Uint32(network)+1)
			ip := first.String()
			if !seen[ip] {
				seen[ip] = true
				out = append(out, ip)
			}
		}
	}
	return out
}

// MakeAllowedSources recomputes the allowlist every 60 s, so a network
// attached after boot is picked up. Not loopback: inside the container that
// is the upstream child. Each IPv4 also allows its ::ffff: mapped form.
func MakeAllowedSources(extra []string, net0 NetInfo) func() map[string]bool {
	var mu sync.Mutex
	var cached map[string]bool
	var cachedAt time.Time
	return func() map[string]bool {
		mu.Lock()
		defer mu.Unlock()
		if cached != nil && time.Since(cachedAt) < 60*time.Second {
			return cached
		}
		base := append(append(GatewaysFromRoute(net0.RouteText()), FirstHosts(net0.Interfaces())...), extra...)
		set := map[string]bool{}
		for _, ip := range base {
			set[ip] = true
			if v4RE.MatchString(ip) {
				set["::ffff:"+ip] = true
			}
		}
		cached, cachedAt = set, time.Now()
		return set
	}
}

var v4RE = regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+$`)

func IsAllowed(address string, allowed map[string]bool) bool {
	return address != "" && allowed[address]
}

// IsAllowedHost: the Host header must name loopback (or an explicitly
// configured name) — DNS-rebinding protection the socket allowlist cannot
// give. See the TS comment.
func IsAllowedHost(hostHeader string, extra ...string) bool {
	if hostHeader == "" {
		return false
	}
	bare := func(h string) string {
		h = strings.ToLower(strings.TrimSpace(h))
		if i := strings.LastIndex(h, ":"); i > 0 && !strings.HasSuffix(h, "]") {
			if _, err := strconv.Atoi(h[i+1:]); err == nil {
				h = h[:i]
			}
		}
		return h
	}
	allowed := append([]string{"localhost", "127.0.0.1", "[::1]"}, mapStrings(extra, bare)...)
	host := bare(hostHeader)
	for _, a := range allowed {
		if host == a {
			return true
		}
	}
	return false
}

func mapStrings(in []string, f func(string) string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = f(s)
	}
	return out
}

// --- the password ----------------------------------------------------------

// scrypt parameters, identical to the TS: N=16384 r=8 p=1 keylen=32, and the
// stored format scrypt$N$r$p$<salt b64>$<key b64> — what the hash-password
// command prints. A TS-produced hash must verify here byte-compatibly.
const (
	scryptN      = 16384
	scryptR      = 8
	scryptP      = 1
	scryptKeyLen = 32
	scryptMaxmem = 64 * 1024 * 1024 // TS maxmem, used by usableHash's model
)

// HashPassword prints one hash for storing in the vault.
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := scrypt.Key([]byte(password), salt, scryptN, scryptR, scryptP, scryptKeyLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("scrypt$%d$%d$%d$%s$%s",
		scryptN, scryptR, scryptP,
		base64.StdEncoding.EncodeToString(salt),
		base64.StdEncoding.EncodeToString(key)), nil
}

var hashRE = regexp.MustCompile(`^scrypt\$(\d+)\$(\d+)\$(\d+)\$([A-Za-z0-9+/=]+)\$([A-Za-z0-9+/=]+)$`)

// VerifyPassword checks a password against a stored hash, constant-time.
func VerifyPassword(password, encoded string) bool {
	m := hashRE.FindStringSubmatch(encoded)
	if m == nil || !UsableHash(encoded) {
		return false
	}
	n, _ := strconv.Atoi(m[1])
	r, _ := strconv.Atoi(m[2])
	p, _ := strconv.Atoi(m[3])
	salt, err := base64.StdEncoding.DecodeString(m[4])
	if err != nil {
		return false
	}
	expected, err := base64.StdEncoding.DecodeString(m[5])
	if err != nil || len(expected) == 0 {
		return false
	}
	key, err := scrypt.Key([]byte(password), salt, n, r, p, len(expected))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(key, expected) == 1
}

// UsableHash: whether a stored hash can ever verify — parameters scrypt would
// reject, or that would make one login attempt a memory bomb, say so.
func UsableHash(encoded string) bool {
	m := hashRE.FindStringSubmatch(encoded)
	if m == nil {
		return false
	}
	key, err := base64.StdEncoding.DecodeString(m[5])
	if err != nil || len(key) == 0 {
		return false
	}
	n, _ := strconv.Atoi(m[1])
	r, _ := strconv.Atoi(m[2])
	p, _ := strconv.Atoi(m[3])
	return n >= 2 && (n&(n-1)) == 0 &&
		r >= 1 && r <= 16 &&
		p >= 1 && p <= 4 &&
		128*r*(n+p+2) <= scryptMaxmem/2
}

// --- the lockout -----------------------------------------------------------

const lockoutWindowMs = 15 * 60_000

type limiterEntry struct {
	times       []int64
	lockedUntil int64
}

// LoginLimiter: five failures lock a source for fifteen minutes, checked
// before scrypt runs. One scrypt at a time: a login flood must not starve
// the MCP relay's quarter-CPU.
type LoginLimiter struct {
	mu       sync.Mutex
	failures map[string]*limiterEntry
	inFlight bool
	now      func() int64
}

func NewLoginLimiter() *LoginLimiter {
	return &LoginLimiter{failures: map[string]*limiterEntry{}, now: func() int64 { return time.Now().UnixMilli() }}
}

func (l *LoginLimiter) Locked(source string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.failures[source]
	return e != nil && e.lockedUntil > l.now()
}

// Fail returns true when this failure starts a lock.
func (l *LoginLimiter) Fail(source string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	at := l.now()
	e := l.failures[source]
	if e == nil {
		e = &limiterEntry{}
	}
	if e.lockedUntil != 0 && e.lockedUntil <= at {
		e.times = nil
		e.lockedUntil = 0
	}
	kept := e.times[:0]
	for _, t := range e.times {
		if at-t < lockoutWindowMs {
			kept = append(kept, t)
		}
	}
	e.times = append(kept, at)
	locks := len(e.times) >= 5 && e.lockedUntil == 0
	if locks {
		e.lockedUntil = at + lockoutWindowMs
	}
	l.failures[source] = e
	return locks
}

func (l *LoginLimiter) Success(source string) {
	l.mu.Lock()
	delete(l.failures, source)
	l.mu.Unlock()
}

func (l *LoginLimiter) Acquire() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlight {
		return false
	}
	l.inFlight = true
	return true
}

func (l *LoginLimiter) Release() {
	l.mu.Lock()
	l.inFlight = false
	l.mu.Unlock()
}

// --- sessions --------------------------------------------------------------

// Session is one dashboard login. Pending holds freshly minted token
// plaintexts by token id, shown once.
type Session struct {
	ID      string
	CSRF    string
	Created int64
	Seen    int64
	Flash   []string
	Pending map[int64]string
}

const (
	idleMs     = 30 * 60_000
	absoluteMs = 8 * 60 * 60_000
)

// SessionStore is in memory: logout really ends a session, and no signing key
// has to live in the vault. A restart logs everyone out.
type SessionStore struct {
	mu       sync.Mutex
	sessions map[string]*Session
	now      func() int64
}

func NewSessionStore() *SessionStore {
	return &SessionStore{sessions: map[string]*Session{}, now: func() int64 { return time.Now().UnixMilli() }}
}

func base64url(n int) string {
	raw := make([]byte, n)
	_, _ = rand.Read(raw)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func (s *SessionStore) Create() *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	at := s.now()
	session := &Session{
		ID:      base64url(32),
		CSRF:    base64url(32),
		Created: at,
		Seen:    at,
		Pending: map[int64]string{},
	}
	s.sessions[session.ID] = session
	return session
}

func (s *SessionStore) expired(sess *Session) bool {
	at := s.now()
	return at-sess.Seen > idleMs || at-sess.Created > absoluteMs
}

func (s *SessionStore) Get(id string) *Session {
	if id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.sessions[id]
	if session == nil {
		return nil
	}
	if s.expired(session) {
		delete(s.sessions, id)
		return nil
	}
	session.Seen = s.now()
	return session
}

func (s *SessionStore) Destroy(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

func (s *SessionStore) Sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, session := range s.sessions {
		if s.expired(session) {
			delete(s.sessions, id)
		}
	}
}
