package main

import (
	"testing"
	"time"
)

func TestBridgeOnlyAcceptsLoopbackEndpoints(t *testing.T) {
	for _, raw := range []string{"http://127.0.0.1:3210", "http://[::1]:3210", "https://localhost:3210"} {
		if _, err := newBridgeClient(raw, "token"); err != nil {
			t.Fatalf("newBridgeClient(%q): %v", raw, err)
		}
	}
	for _, raw := range []string{"https://example.com", "http://192.168.1.10:3210"} {
		if _, err := newBridgeClient(raw, "token"); err == nil {
			t.Fatalf("newBridgeClient(%q) unexpectedly succeeded", raw)
		}
	}
}

func TestDeduplicatorExpiresEntries(t *testing.T) {
	var dedup deduplicator
	now := time.Now()
	if !dedup.accept("telegram:1", now) || dedup.accept("telegram:1", now.Add(time.Second)) {
		t.Fatal("deduplicator did not reject duplicate")
	}
	if !dedup.accept("telegram:1", now.Add(duplicateTTL+time.Second)) {
		t.Fatal("deduplicator did not expire entry")
	}
}
