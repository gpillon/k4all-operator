package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"time"
)

// sessions issues and checks IMDSv2 session tokens. A token carries its
// expiry and is signed with a key of this process: nothing is stored, and
// a restarted server (or another VM's) does not take it, as EC2 does not
// after a stop.
type sessions struct {
	key []byte
	now func() time.Time
}

// Lengths in a token: expiry (unix seconds), nonce, MAC.
const (
	expiryLen = 8
	nonceLen  = 8
	macLen    = 16
)

func newSessions(key []byte, now func() time.Time) *sessions {
	if len(key) == 0 {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			panic("no randomness for the session key: " + err.Error())
		}
	}
	return &sessions{key: key, now: now}
}

func (s *sessions) issue(ttl time.Duration) string {
	payload := make([]byte, expiryLen+nonceLen, expiryLen+nonceLen+macLen)
	binary.BigEndian.PutUint64(payload, uint64(s.now().Add(ttl).Unix()))
	_, _ = rand.Read(payload[expiryLen:])
	return base64.RawURLEncoding.EncodeToString(append(payload, s.mac(payload)...))
}

func (s *sessions) valid(token string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != expiryLen+nonceLen+macLen {
		return false
	}
	payload, mac := raw[:expiryLen+nonceLen], raw[expiryLen+nonceLen:]
	if !hmac.Equal(mac, s.mac(payload)) {
		return false
	}
	return s.now().Unix() < int64(binary.BigEndian.Uint64(payload))
}

func (s *sessions) mac(payload []byte) []byte {
	h := hmac.New(sha256.New, s.key)
	h.Write(payload)
	return h.Sum(nil)[:macLen]
}
