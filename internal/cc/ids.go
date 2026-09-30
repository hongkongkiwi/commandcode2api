package cc

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// CCProtocolVersion is the wire protocol version this proxy actually
// implements (aligned with command-code@1.53.1 source). Newer npm releases
// only raise a drift warning — never a silent version bump.
const CCProtocolVersion = "1.53.1"

// NewUUID returns a random (v4-shaped) UUID string.
func NewUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is unrecoverable; panic like the runtime would.
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// UUID12 mirrors randomUUID().slice(0, 12) (hyphens included).
func UUID12() string { return NewUUID()[:12] }

// NewResponsesID mirrors newResponsesId: prefix + first 24 chars of a
// dash-less UUID.
func NewResponsesID(prefix string) string {
	return prefix + hex.EncodeToString([]byte(NewUUID()[:12]))[:0] + dashless24()
}

func dashless24() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b[:])[:24]
}

// RandHex returns n random bytes hex-encoded.
func RandHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b)
}

// GenerateTraceparent returns a W3C Trace Context header value.
func GenerateTraceparent() string {
	return "00-" + RandHex(16) + "-" + RandHex(8) + "-01"
}

// NowUnix returns the current unix time in seconds.
func NowUnix() int64 { return time.Now().Unix() }
