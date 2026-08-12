package main

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	zhiyuanProtocolVersion = "1"
	protocolClockSkew      = 5 * time.Minute
)

type nonceCache struct {
	mu      sync.Mutex
	entries map[string]time.Time
}

func (c *nonceCache) accept(nonce string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]time.Time)
	}
	for value, expiresAt := range c.entries {
		if !expiresAt.After(now) {
			delete(c.entries, value)
		}
	}
	if _, exists := c.entries[nonce]; exists {
		return false
	}
	c.entries[nonce] = now.Add(protocolClockSkew)
	return true
}

type requestAuthenticator struct {
	token  string
	nonces nonceCache
	now    func() time.Time
}

func (a *requestAuthenticator) authorize(request *http.Request) error {
	if !secureBearerMatch(request.Header.Get("Authorization"), a.token) {
		return errors.New("invalid bearer token")
	}
	if request.Header.Get("X-ZhiYuan-Protocol-Version") != zhiyuanProtocolVersion {
		return errors.New("unsupported protocol version")
	}
	timestampMs, err := strconv.ParseInt(request.Header.Get("X-ZhiYuan-Timestamp-Ms"), 10, 64)
	if err != nil {
		return errors.New("invalid request timestamp")
	}
	now := time.Now().UTC()
	if a.now != nil {
		now = a.now().UTC()
	}
	timestamp := time.UnixMilli(timestampMs)
	if timestamp.Before(now.Add(-protocolClockSkew)) || timestamp.After(now.Add(protocolClockSkew)) {
		return errors.New("request timestamp outside accepted window")
	}
	nonce := strings.TrimSpace(request.Header.Get("X-ZhiYuan-Nonce"))
	if nonce == "" || !a.nonces.accept(nonce, now) {
		return errors.New("missing or replayed request nonce")
	}
	return nil
}

func addProtocolHeaders(request *http.Request, requestID string) error {
	nonce, err := newRequestID()
	if err != nil {
		return err
	}
	request.Header.Set("X-ZhiYuan-Protocol-Version", zhiyuanProtocolVersion)
	request.Header.Set("X-ZhiYuan-Request-ID", requestID)
	request.Header.Set("X-ZhiYuan-Timestamp-Ms", strconv.FormatInt(time.Now().UTC().UnixMilli(), 10))
	request.Header.Set("X-ZhiYuan-Nonce", nonce)
	return nil
}

type healthResponse struct {
	ProtocolVersion string   `json:"protocolVersion"`
	PID             int      `json:"pid"`
	ParentPID       int      `json:"parentPid"`
	Capabilities    []string `json:"capabilities"`
}

func currentHealth() healthResponse {
	return healthResponse{
		ProtocolVersion: zhiyuanProtocolVersion,
		PID:             os.Getpid(),
		ParentPID:       os.Getppid(),
		Capabilities:    []string{"channel-transport", "delivery", "trigger-only-cron"},
	}
}

func secureBearerMatch(value, token string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	provided := []byte(strings.TrimPrefix(value, prefix))
	expected := []byte(token)
	return len(provided) == len(expected) && subtle.ConstantTimeCompare(provided, expected) == 1
}
