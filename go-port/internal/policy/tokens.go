package policy

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
)

// HashToken is sha256 hex of the plaintext bearer.
func HashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// SafeEqual compares two strings in constant time. Both sides are digested
// first: a constant-time compare throws (or leaks) on a length mismatch, which
// would itself be the leak.
func SafeEqual(a, b string) bool {
	x := sha256.Sum256([]byte(a))
	y := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(x[:], y[:]) == 1
}

// MintToken creates a bearer for one agent: "gws_" + 32 random bytes,
// base64url. 256 random bits, so a plain sha256 is the right storage: there
// is nothing to brute-force that a slow hash would protect. Only the hash and
// a short prefix are kept.
func MintToken() (plaintext, hash, prefix string, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", "", "", err
	}
	plaintext = "gws_" + base64.RawURLEncoding.EncodeToString(buf)
	return plaintext, HashToken(plaintext), plaintext[:12], nil
}
