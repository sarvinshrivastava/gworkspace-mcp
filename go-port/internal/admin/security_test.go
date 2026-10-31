package admin

import (
	"net"
	"strings"
	"testing"
)

// TS vector: produced by the pinned repo's own hashPassword path (Node
// crypto.scrypt, N=16384 r=8 p=1 keylen=32, salt "0123456789abcdef").
// The Go verifier must accept TS-produced hashes byte-compatibly.
const tsVector = "scrypt$16384$8$1$MDEyMzQ1Njc4OWFiY2RlZg==$tjK03tRvEjqCcPwmgtddMkgjlXrk8U/b9rIvfeBMKCc="
const tsPassword = "correct horse battery staple"

func TestVerifyPassword_TSVector(t *testing.T) {
	if !VerifyPassword(tsPassword, tsVector) {
		t.Fatal("Go scrypt rejected a TS-produced hash")
	}
	if VerifyPassword("wrong password", tsVector) {
		t.Fatal("wrong password verified")
	}
}

func TestHashPassword_RoundTrip(t *testing.T) {
	h, err := HashPassword("päss wörd 🔐")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "scrypt$16384$8$1$") {
		t.Fatalf("format: %s", h)
	}
	if !hashRE.MatchString(h) {
		t.Fatalf("regex: %s", h)
	}
	if !VerifyPassword("päss wörd 🔐", h) {
		t.Fatal("round trip failed")
	}
}

func TestUsableHash(t *testing.T) {
	if !UsableHash(tsVector) {
		t.Fatal("the real hash is usable")
	}
	cases := map[string]bool{
		"garbage": false,
		"scrypt$3$8$1$MDEyMzQ1Njc4OWFiY2RlZg==$dGpLMzA=":       false, // N not a power of two
		"scrypt$1048576$8$1$MDEyMzQ1Njc4OWFiY2RlZg==$dGpLMzA=": false, // memory bomb
		"scrypt$16384$0$1$MDEyMzQ1Njc4OWFiY2RlZg==$dGpLMzA=":   false, // r=0
		"scrypt$16384$8$5$MDEyMzQ1Njc4OWFiY2RlZg==$dGpLMzA=":   false, // p>4
		"scrypt$16384$8$1$MDEyMzQ1Njc4OWFiY2RlZg==$":           false, // empty key
	}
	for h, _ := range cases {
		if UsableHash(h) {
			t.Fatalf("usable: %s", h)
		}
	}
}

func TestLoginLimiter(t *testing.T) {
	var clock int64 = 1_000_000
	l := NewLoginLimiter()
	l.now = func() int64 { return clock }
	src := "10.0.0.1"
	for i := 0; i < 4; i++ {
		if l.Fail(src) {
			t.Fatalf("locked after %d failures", i+1)
		}
	}
	if !l.Fail(src) {
		t.Fatal("5th failure must start the lock")
	}
	if !l.Locked(src) {
		t.Fatal("locked")
	}
	// Sliding window: failures older than 15 minutes stop counting.
	clock += lockoutWindowMs + 1
	if l.Locked(src) {
		t.Fatal("lock expired")
	}
	if l.Fail(src) {
		t.Fatal("old failures must not count after the window")
	}
	// Success clears.
	l.Success(src)
	if l.Locked(src) {
		t.Fatal("success clears")
	}
	// One scrypt at a time.
	if !l.Acquire() {
		t.Fatal("first acquire")
	}
	if l.Acquire() {
		t.Fatal("second acquire while in flight")
	}
	l.Release()
	if !l.Acquire() {
		t.Fatal("acquire after release")
	}
}

func TestSessionStore(t *testing.T) {
	var clock int64 = 1_000_000
	s := NewSessionStore()
	s.now = func() int64 { return clock }
	sess := s.Create()
	if s.Get(sess.ID) != sess {
		t.Fatal("get")
	}
	if s.Get("") != nil || s.Get("nope") != nil {
		t.Fatal("missing ids")
	}
	// Idle expiry.
	clock += idleMs + 1
	if s.Get(sess.ID) != nil {
		t.Fatal("idle expiry")
	}
	// Absolute expiry even with activity.
	sess2 := s.Create()
	clock += absoluteMs + 1
	if s.Get(sess2.ID) != nil {
		t.Fatal("absolute expiry")
	}
	sess3 := s.Create()
	s.Destroy(sess3.ID)
	if s.Get(sess3.ID) != nil {
		t.Fatal("destroy")
	}
}

func TestGatewaysFromRoute(t *testing.T) {
	route := `Iface	Destination	Gateway	Flags	RefCnt	Use	Metric	Mask	MTU	Window	IRTT
eth0	00000000	0102A8C0	0003	0	0	0	00000000	0	0	0
eth0	0002A8C0	00000000	0001	0	0	0	00FFFFFF	0	0	0
`
	got := GatewaysFromRoute(route)
	if len(got) != 1 || got[0] != "192.168.2.1" {
		t.Fatalf("gateways = %v", got)
	}
}

func TestIsAllowedHost(t *testing.T) {
	for _, ok := range map[string]bool{
		"localhost":        true,
		"localhost:3015":   true,
		"127.0.0.1":        true,
		"127.0.0.1:3015":   true,
		"[::1]":            true,
		"LOCALHOST":        true,
		"evil.example.com": false,
		"":                 false,
	} {
		if IsAllowedHost(ok2name(ok)) != ok {
			t.Fatalf("host case failed")
		}
	}
	if !IsAllowedHost("admin.internal:3015", "admin.internal") {
		t.Fatal("extra host")
	}
}

func ok2name(b bool) string {
	if b {
		return "localhost"
	}
	return "evil.example.com"
}

func TestParseFolderInput(t *testing.T) {
	id := strings.Repeat("a", 20)
	cases := map[string]string{
		id: id,
		"https://drive.google.com/drive/folders/" + id: id,
		"https://x/?id=" + id:                          id,
		"not a folder":                                 "",
		"short":                                        "",
	}
	for in, want := range cases {
		got := ParseFolderInput(in)
		if want == "" {
			if got != nil {
				t.Fatalf("%q → %v", in, *got)
			}
		} else if got == nil || *got != want {
			t.Fatalf("%q → %v", in, got)
		}
	}
}

// TS security.ts firstHosts: (network + 1) >>> 0 is a 32-bit increment.
// A network address ending in .255 (a /32 on .255, or an unusual mask) must
// carry into the next octet, not wrap within the last one.
func TestFirstHosts_Carry32Bit(t *testing.T) {
	_, ipnet, err := net.ParseCIDR("10.0.0.255/32")
	if err != nil {
		t.Fatal(err)
	}
	got := FirstHosts(map[string][]net.Addr{"eth0": {ipnet}})
	if len(got) != 1 || got[0] != "10.0.1.0" {
		t.Fatalf("FirstHosts(/32 on .255) = %v, want [10.0.1.0]", got)
	}
	_, ipnet2, _ := net.ParseCIDR("172.18.0.0/16")
	got2 := FirstHosts(map[string][]net.Addr{"eth0": {ipnet2}})
	if len(got2) != 1 || got2[0] != "172.18.0.1" {
		t.Fatalf("FirstHosts(/16) = %v, want [172.18.0.1]", got2)
	}
}
