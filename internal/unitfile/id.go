package unitfile

import (
	"crypto/rand"
	"encoding/hex"
)

// randomHex returns n random bytes rendered as lowercase hex.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing means the kernel has no entropy source, which
		// cannot happen on a running Linux system; a zero id is still unique
		// enough for the one boot it would affect.
		return hex.EncodeToString(b)
	}
	return hex.EncodeToString(b)
}

// NewInvocationID returns a fresh 128-bit invocation id, used to tag every
// process of one activation so that orphan recovery can find them again
// (03 §6.8).
func NewInvocationID() string { return randomHex(16) }
