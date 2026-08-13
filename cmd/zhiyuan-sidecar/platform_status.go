package main

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

const (
	platformStateStarting    = "starting"
	platformStateReady       = "ready"
	platformStateUnavailable = "unavailable"
)

type platformRuntimeStatus struct {
	AccountID      string `json:"accountId"`
	Platform       string `json:"platform"`
	State          string `json:"state"`
	LastError      string `json:"lastError,omitempty"`
	StartedAt      string `json:"startedAt,omitempty"`
	LastInboundAt  string `json:"lastInboundAt,omitempty"`
	LastOutboundAt string `json:"lastOutboundAt,omitempty"`
}

type platformStatusRegistry struct {
	mu       sync.RWMutex
	statuses map[string]platformRuntimeStatus
}

func newPlatformStatusRegistry() *platformStatusRegistry {
	return &platformStatusRegistry{statuses: make(map[string]platformRuntimeStatus)}
}

func (r *platformStatusRegistry) set(accountID, platform, state string, err error) {
	key := deliveryPlatformKey(strings.TrimSpace(accountID), strings.TrimSpace(platform))
	r.mu.Lock()
	previous := r.statuses[key]
	status := platformRuntimeStatus{
		AccountID:      strings.TrimSpace(accountID),
		Platform:       strings.TrimSpace(platform),
		State:          state,
		StartedAt:      previous.StartedAt,
		LastInboundAt:  previous.LastInboundAt,
		LastOutboundAt: previous.LastOutboundAt,
	}
	if state == platformStateReady && previous.State != platformStateReady {
		status.StartedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if err != nil {
		status.LastError = err.Error()
	}
	r.statuses[key] = status
	r.mu.Unlock()
}

func (r *platformStatusRegistry) markInbound(accountID, platform string) {
	r.markActivity(accountID, platform, true)
}

func (r *platformStatusRegistry) markOutbound(accountID, platform string) {
	r.markActivity(accountID, platform, false)
}

func (r *platformStatusRegistry) markActivity(accountID, platform string, inbound bool) {
	key := deliveryPlatformKey(strings.TrimSpace(accountID), strings.TrimSpace(platform))
	r.mu.Lock()
	status, ok := r.statuses[key]
	if ok {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if inbound {
			status.LastInboundAt = now
		} else {
			status.LastOutboundAt = now
		}
		r.statuses[key] = status
	}
	r.mu.Unlock()
}

func (r *platformStatusRegistry) snapshot() []platformRuntimeStatus {
	r.mu.RLock()
	result := make([]platformRuntimeStatus, 0, len(r.statuses))
	for _, status := range r.statuses {
		result = append(result, status)
	}
	r.mu.RUnlock()
	sort.Slice(result, func(i, j int) bool {
		if result[i].AccountID == result[j].AccountID {
			return result[i].Platform < result[j].Platform
		}
		return result[i].AccountID < result[j].AccountID
	})
	return result
}

type projectPlatformLifecycle struct {
	accountID string
	statuses  *platformStatusRegistry
}

func (h *projectPlatformLifecycle) OnPlatformReady(platform core.Platform) {
	h.statuses.set(h.accountID, platform.Name(), platformStateReady, nil)
}

func (h *projectPlatformLifecycle) OnPlatformUnavailable(platform core.Platform, err error) {
	h.statuses.set(h.accountID, platform.Name(), platformStateUnavailable, err)
}
