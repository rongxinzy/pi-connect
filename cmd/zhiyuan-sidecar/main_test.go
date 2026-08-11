package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/config"
	"github.com/chenhg5/cc-connect/core"
)

type deliveryPlatformStub struct{ sent string }

func (p *deliveryPlatformStub) Name() string                             { return "telegram" }
func (p *deliveryPlatformStub) Start(core.MessageHandler) error          { return nil }
func (p *deliveryPlatformStub) Reply(context.Context, any, string) error { return nil }
func (p *deliveryPlatformStub) Send(_ context.Context, _ any, content string) error {
	p.sent = content
	return nil
}
func (p *deliveryPlatformStub) Stop() error { return nil }
func (p *deliveryPlatformStub) ReconstructReplyCtx(sessionKey string) (any, error) {
	return sessionKey, nil
}

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
	handler := cronControlHandler("secret", controller, nil)

	unauthorized := httptest.NewRequest(http.MethodPost, "/v1/cc-connect/cron/tasks", nil)
	unauthorizedResponse := httptest.NewRecorder()
	handler.ServeHTTP(unauthorizedResponse, unauthorized)
	if unauthorizedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorizedResponse.Code)
	}

	health := httptest.NewRequest(http.MethodGet, "/v1/cc-connect/cron/health", nil)
	health.Header.Set("Authorization", "Bearer secret")
	healthResponse := httptest.NewRecorder()
	handler.ServeHTTP(healthResponse, health)
	if healthResponse.Code != http.StatusNoContent {
		t.Fatalf("health status = %d", healthResponse.Code)
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

func TestDeliveryControlUsesOnlyConfiguredProactivePlatform(t *testing.T) {
	controller := newCronController("project", &bridgeClient{})
	sender := &deliverySender{}
	platform := &deliveryPlatformStub{}
	sender.register(platform)
	handler := cronControlHandler("secret", controller, sender)

	request := httptest.NewRequest(http.MethodPost, "/v1/cc-connect/deliver", bytes.NewBufferString(`{"platform":"telegram","sessionKey":"telegram:42","content":"done"}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || platform.sent != "done" {
		t.Fatalf("delivery result status=%d content=%q", response.Code, platform.sent)
	}

	bad := httptest.NewRequest(http.MethodPost, "/v1/cc-connect/deliver", bytes.NewBufferString(`{"platform":"telegram","sessionKey":"telegram:42","content":"done","exec":"whoami"}`))
	bad.Header.Set("Authorization", "Bearer secret")
	badResponse := httptest.NewRecorder()
	handler.ServeHTTP(badResponse, bad)
	if badResponse.Code != http.StatusBadRequest {
		t.Fatalf("unknown delivery field status=%d", badResponse.Code)
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

func TestZhiyuanSchedulerOnlyConfigContractLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zhiyuan-scheduler-sidecar.toml")
	contents := `data_dir = "C:\\Users\\test\\AppData\\Local\\ZhiYuanAgent\\cc-connect"

[webhook]
enabled = false

[bridge]
enabled = false

[management]
enabled = false

[[projects]]
name = "__zhiyuan_scheduler__"
[projects.agent]
type = "zhiyuan-bridge"
[projects.agent.options]
bridge_url = "http://127.0.0.1:34567"
bridge_token = "secret"
cron_control_listen = "127.0.0.1:0"
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load scheduler-only ZhiYuan sidecar contract: %v", err)
	}
	if len(cfg.Projects) != 1 || len(cfg.Projects[0].Platforms) != 0 {
		t.Fatalf("unexpected scheduler-only project contract: %#v", cfg.Projects)
	}
}
