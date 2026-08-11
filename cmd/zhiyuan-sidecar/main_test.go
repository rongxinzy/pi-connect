package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/config"
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
		bytes.NewBufferString(`{"taskId":"a","scheduleVersion":"v1","schedule":{"kind":"cron","expr":"0 9 * * *"},"exec":"whoami"}`),
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
	if err := controller.upsert(cronTaskRequest{TaskID: "task", ScheduleVersion: "v1", Schedule: cronSchedule{Kind: "cron", Expr: "not a cron"}}); err == nil {
		t.Fatal("invalid cron expression was accepted")
	}
	if err := controller.upsert(cronTaskRequest{TaskID: "task", ScheduleVersion: "v1", Schedule: cronSchedule{Kind: "cron", Expr: "0 9 * * *", Timezone: "Asia/Shanghai"}}); err != nil {
		t.Fatalf("valid cron registration: %v", err)
	}
	if err := controller.upsert(cronTaskRequest{TaskID: "interval", ScheduleVersion: "v1", Schedule: cronSchedule{Kind: "every", EveryMs: 1}}); err != nil {
		t.Fatalf("valid interval registration: %v", err)
	}
	if err := controller.upsert(cronTaskRequest{TaskID: "once", ScheduleVersion: "v1", Schedule: cronSchedule{Kind: "at", At: time.Now().Add(time.Hour).Format(time.RFC3339)}}); err != nil {
		t.Fatalf("valid one-time registration: %v", err)
	}
	if !controller.remove("task") || controller.remove("task") {
		t.Fatal("remove did not preserve expected idempotency")
	}
	controller.remove("interval")
	controller.remove("once")
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

// This is the versioned contract emitted by RongxinAI's
// CcConnectSidecarConfig serializer. The agent type is intentionally never
// instantiated by this command; it only carries bridge control-plane options.
func TestZhiyuanBridgeConfigContractLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zhiyuan-sidecar.toml")
	contents := `data_dir = "C:\\Users\\test\\AppData\\Local\\ZhiYuanAgent\\cc-connect"

[webhook]
enabled = false

[bridge]
enabled = false

[management]
enabled = false

[[projects]]
name = "telegram-primary"
[projects.agent]
type = "zhiyuan-bridge"
[projects.agent.options]
bridge_url = "http://127.0.0.1:34567"
bridge_token = "secret"
cron_control_listen = "127.0.0.1:0"
[[projects.platforms]]
type = "telegram"
[projects.platforms.options]
token = "token"
allow_from = ["u1"]
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load ZhiYuan sidecar contract: %v", err)
	}
	if len(cfg.Projects) != 1 || cfg.Projects[0].Agent.Type != "zhiyuan-bridge" {
		t.Fatalf("unexpected project contract: %#v", cfg.Projects)
	}
}
