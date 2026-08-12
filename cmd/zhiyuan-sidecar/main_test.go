package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/config"
	"github.com/chenhg5/cc-connect/core"
)

func authorizeTestRequest(request *http.Request, nonce string) {
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("X-ZhiYuan-Protocol-Version", zhiyuanProtocolVersion)
	request.Header.Set("X-ZhiYuan-Timestamp-Ms", strconv.FormatInt(time.Now().UnixMilli(), 10))
	request.Header.Set("X-ZhiYuan-Nonce", nonce)
}

type deliveryPlatformStub struct{ name, sent string }

func (p *deliveryPlatformStub) Name() string {
	if p.name != "" {
		return p.name
	}
	return "telegram"
}
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
	handler := cronControlHandler("secret", newCronControllerRegistry([]*cronController{controller}), nil, nil)

	unauthorized := httptest.NewRequest(http.MethodPost, "/v1/cc-connect/cron/tasks", nil)
	unauthorizedResponse := httptest.NewRecorder()
	handler.ServeHTTP(unauthorizedResponse, unauthorized)
	if unauthorizedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorizedResponse.Code)
	}

	health := httptest.NewRequest(http.MethodGet, "/v1/cc-connect/cron/health", nil)
	authorizeTestRequest(health, "health-nonce")
	healthResponse := httptest.NewRecorder()
	handler.ServeHTTP(healthResponse, health)
	if healthResponse.Code != http.StatusOK {
		t.Fatalf("health status = %d", healthResponse.Code)
	}
	if !bytes.Contains(healthResponse.Body.Bytes(), []byte(`"protocolVersion":"1"`)) {
		t.Fatalf("health body = %s", healthResponse.Body.String())
	}

	execPayload := httptest.NewRequest(
		http.MethodPost,
		"/v1/cc-connect/cron/tasks",
		bytes.NewBufferString(`{"taskId":"a","scheduleVersion":"v1","schedule":{"kind":"cron","expr":"0 9 * * *"},"exec":"whoami"}`),
	)
	authorizeTestRequest(execPayload, "exec-nonce")
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
	sender.register("account", platform)
	handler := cronControlHandler("secret", newCronControllerRegistry([]*cronController{controller}), sender, nil)

	request := httptest.NewRequest(http.MethodPost, "/v1/cc-connect/deliver", bytes.NewBufferString(`{"accountId":"account","platform":"telegram","sessionKey":"telegram:42","content":"done"}`))
	authorizeTestRequest(request, "delivery-nonce")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || platform.sent != "done" {
		t.Fatalf("delivery result status=%d content=%q", response.Code, platform.sent)
	}

	bad := httptest.NewRequest(http.MethodPost, "/v1/cc-connect/deliver", bytes.NewBufferString(`{"accountId":"account","platform":"telegram","sessionKey":"telegram:42","content":"done","exec":"whoami"}`))
	authorizeTestRequest(bad, "bad-delivery-nonce")
	badResponse := httptest.NewRecorder()
	handler.ServeHTTP(badResponse, bad)
	if badResponse.Code != http.StatusBadRequest {
		t.Fatalf("unknown delivery field status=%d", badResponse.Code)
	}
}

func TestDeliverySenderRoutesByAccountAndPlatform(t *testing.T) {
	sender := &deliverySender{}
	first := &deliveryPlatformStub{name: "dingtalk"}
	second := &deliveryPlatformStub{name: "dingtalk"}
	sender.register("first", first)
	sender.register("second", second)
	if err := sender.send(deliveryRequest{AccountID: "second", Platform: "dingtalk", SessionKey: "dingtalk:g:chat", Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	if first.sent != "" || second.sent != "hello" {
		t.Fatalf("unexpected routing first=%q second=%q", first.sent, second.sent)
	}
}

func TestCronControlRejectsReplayedNonce(t *testing.T) {
	controller := newCronController("project", &bridgeClient{})
	handler := cronControlHandler("secret", newCronControllerRegistry([]*cronController{controller}), nil, nil)
	for attempt := 0; attempt < 2; attempt++ {
		request := httptest.NewRequest(http.MethodGet, "/v1/cc-connect/cron/health", nil)
		authorizeTestRequest(request, "same-nonce")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		want := http.StatusOK
		if attempt == 1 {
			want = http.StatusUnauthorized
		}
		if response.Code != want {
			t.Fatalf("attempt %d status=%d want=%d", attempt, response.Code, want)
		}
	}
}

func TestCronControlRoutesTasksByAccount(t *testing.T) {
	first := newCronController("first", &bridgeClient{})
	second := newCronController("second", &bridgeClient{})
	handler := cronControlHandler(
		"secret",
		newCronControllerRegistry([]*cronController{first, second}),
		nil,
		nil,
	)

	upsert := httptest.NewRequest(
		http.MethodPost,
		"/v1/cc-connect/cron/tasks",
		bytes.NewBufferString(`{"accountId":"second","taskId":"task","scheduleVersion":"v1","schedule":{"kind":"at","at":"2099-01-01T00:00:00Z"}}`),
	)
	authorizeTestRequest(upsert, "route-upsert")
	upsertResponse := httptest.NewRecorder()
	handler.ServeHTTP(upsertResponse, upsert)
	if upsertResponse.Code != http.StatusNoContent {
		t.Fatalf("upsert status=%d body=%s", upsertResponse.Code, upsertResponse.Body.String())
	}
	if len(first.jobs) != 0 || len(second.jobs) != 1 {
		t.Fatalf("unexpected routed jobs first=%d second=%d", len(first.jobs), len(second.jobs))
	}

	remove := httptest.NewRequest(http.MethodDelete, "/v1/cc-connect/cron/tasks/task?accountId=second", nil)
	authorizeTestRequest(remove, "route-remove")
	removeResponse := httptest.NewRecorder()
	handler.ServeHTTP(removeResponse, remove)
	if removeResponse.Code != http.StatusNoContent || len(second.jobs) != 0 {
		t.Fatalf("remove status=%d second jobs=%d", removeResponse.Code, len(second.jobs))
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

func TestCronControllerRecoversPastAtWithScheduledOccurrence(t *testing.T) {
	controller := newCronController("project", &bridgeClient{}, t.TempDir())
	scheduledAt := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	if err := controller.upsert(cronTaskRequest{TaskID: "once", ScheduleVersion: "v1", Schedule: cronSchedule{Kind: "at", At: scheduledAt.Format(time.RFC3339)}}); err != nil {
		t.Fatalf("past at registration: %v", err)
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if len(controller.pending) != 1 {
		t.Fatalf("pending count=%d", len(controller.pending))
	}
	for _, trigger := range controller.pending {
		if !trigger.ScheduledAt.Equal(scheduledAt) {
			t.Fatalf("scheduledAt=%s want=%s", trigger.ScheduledAt, scheduledAt)
		}
	}
}

func TestCronOutboxSurvivesRestart(t *testing.T) {
	dataDir := t.TempDir()
	scheduledAt := time.Now().UTC().Truncate(time.Millisecond)
	first := newCronController("project", &bridgeClient{}, dataDir)
	first.trigger("task", "v1", scheduledAt)
	second := newCronController("project", &bridgeClient{}, dataDir)
	second.mu.Lock()
	if err := second.loadOutboxLocked(); err != nil {
		second.mu.Unlock()
		t.Fatal(err)
	}
	if len(second.pending) != 1 {
		second.mu.Unlock()
		t.Fatalf("pending count=%d", len(second.pending))
	}
	for _, trigger := range second.pending {
		if !trigger.ScheduledAt.Equal(scheduledAt) {
			second.mu.Unlock()
			t.Fatalf("scheduledAt=%s want=%s", trigger.ScheduledAt, scheduledAt)
		}
	}
	second.mu.Unlock()
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

func TestBridgeChannelIDPrefersWorkspaceChannelKey(t *testing.T) {
	message := &core.Message{ChannelID: "legacy-channel", ChannelKey: "native-channel"}
	if got := bridgeChannelID(message); got != "native-channel" {
		t.Fatalf("bridgeChannelID() = %q, want native-channel", got)
	}
	message.ChannelKey = ""
	if got := bridgeChannelID(message); got != "legacy-channel" {
		t.Fatalf("bridgeChannelID() fallback = %q, want legacy-channel", got)
	}
}
