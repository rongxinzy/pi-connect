package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
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

func TestCronControlRejectsUnauthenticatedAndExecPayloads(t *testing.T) {
	controller := newCronController("project", &bridgeClient{})
	handler := cronControlHandler("secret", controller)

	unauthorized := httptest.NewRequest(http.MethodPost, "/v1/cc-connect/cron/tasks", nil)
	unauthorizedResponse := httptest.NewRecorder()
	handler.ServeHTTP(unauthorizedResponse, unauthorized)
	if unauthorizedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorizedResponse.Code)
	}

	execPayload := httptest.NewRequest(
		http.MethodPost,
		"/v1/cc-connect/cron/tasks",
		bytes.NewBufferString(`{"taskId":"a","scheduleVersion":"v1","expression":"0 9 * * *","exec":"whoami"}`),
	)
	execPayload.Header.Set("Authorization", "Bearer secret")
	execResponse := httptest.NewRecorder()
	handler.ServeHTTP(execResponse, execPayload)
	if execResponse.Code != http.StatusBadRequest {
		t.Fatalf("exec payload status = %d", execResponse.Code)
	}
}

func TestCronControllerValidatesTriggerOnlyRegistration(t *testing.T) {
	controller := newCronController("project", &bridgeClient{})
	if err := controller.upsert(cronTaskRequest{TaskID: "task", ScheduleVersion: "v1", Expression: "not a cron"}); err == nil {
		t.Fatal("invalid cron expression was accepted")
	}
	if err := controller.upsert(cronTaskRequest{TaskID: "task", ScheduleVersion: "v1", Expression: "0 9 * * *", Timezone: "Asia/Shanghai"}); err != nil {
		t.Fatalf("valid cron registration: %v", err)
	}
	if !controller.remove("task") || controller.remove("task") {
		t.Fatal("remove did not preserve expected idempotency")
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
