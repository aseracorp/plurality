package webhook

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
)

// GenerateToken returns 32 random bytes encoded as URL-safe base64 (no
// padding). 256 bits of entropy means a plain sha256 hash is sufficient
// protection at rest — bcrypt-style stretching would be overkill for a
// non-guessable secret.
func GenerateToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read only fails on a broken system. Re-panic so we don't
		// silently issue weak tokens.
		panic("webhook: crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// HashToken returns the sha256 hex digest of a plaintext token.
func HashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// CheckToken validates a presented plaintext token against a stored hash
// in constant time.
func CheckToken(presented, storedHash string) bool {
	got := HashToken(presented)
	return subtle.ConstantTimeCompare([]byte(got), []byte(storedHash)) == 1
}
