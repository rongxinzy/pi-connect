package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
	"github.com/robfig/cron/v3"
)

// cronController contains no durable task state and deliberately has no exec
// surface. ZhiYuan is the sole source of truth and reconciles registrations
// after any sidecar restart.
type cronController struct {
	project    string
	bridge     *bridgeClient
	outboxPath string
	mu         sync.Mutex
	jobs       map[string]func()
	pending    map[string]pendingTrigger
	wake       chan struct{}
	stopCh     chan struct{}
	done       chan struct{}
	started    bool
}

type pendingTrigger struct {
	TaskID          string    `json:"taskId"`
	ScheduleVersion string    `json:"scheduleVersion"`
	ScheduledAt     time.Time `json:"scheduledAt"`
}

type cronTaskRequest struct {
	AccountID       string       `json:"accountId"`
	TaskID          string       `json:"taskId"`
	ScheduleVersion string       `json:"scheduleVersion"`
	Schedule        cronSchedule `json:"schedule"`
}

type cronControllerRegistry struct {
	controllers map[string]*cronController
}

func newCronControllerRegistry(controllers []*cronController) *cronControllerRegistry {
	registry := &cronControllerRegistry{controllers: make(map[string]*cronController, len(controllers))}
	for _, controller := range controllers {
		if controller != nil {
			registry.controllers[controller.project] = controller
		}
	}
	return registry
}

func (r *cronControllerRegistry) resolve(accountID string) (*cronController, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil, errors.New("accountId is required")
	}
	controller := r.controllers[accountID]
	if controller == nil {
		return nil, errors.New("configured project is unavailable")
	}
	return controller, nil
}

// deliveryRequest contains only a resolved outbound target and final text.
// It deliberately excludes prompts, tool inputs, and arbitrary commands.
type deliveryRequest struct {
	AccountID  string `json:"accountId"`
	Platform   string `json:"platform"`
	SessionKey string `json:"sessionKey"`
	Content    string `json:"content"`
}

type deliverySender struct {
	mu        sync.RWMutex
	platforms map[string]core.Platform
}

func (s *deliverySender) register(accountID string, platform core.Platform) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.platforms == nil {
		s.platforms = make(map[string]core.Platform)
	}
	s.platforms[deliveryPlatformKey(accountID, platform.Name())] = platform
}

func (s *deliverySender) send(request deliveryRequest) error {
	if strings.TrimSpace(request.AccountID) == "" || strings.TrimSpace(request.Platform) == "" || strings.TrimSpace(request.SessionKey) == "" || strings.TrimSpace(request.Content) == "" {
		return errors.New("accountId, platform, sessionKey, and content are required")
	}
	s.mu.RLock()
	platform := s.platforms[deliveryPlatformKey(request.AccountID, request.Platform)]
	s.mu.RUnlock()
	if platform == nil {
		return errors.New("configured platform is unavailable")
	}
	reconstructor, ok := platform.(core.ReplyContextReconstructor)
	if !ok {
		return errors.New("platform does not support proactive delivery")
	}
	replyCtx, err := reconstructor.ReconstructReplyCtx(request.SessionKey)
	if err != nil {
		return fmt.Errorf("resolve delivery target: %w", err)
	}
	if err := platform.Send(context.Background(), replyCtx, request.Content); err != nil {
		return fmt.Errorf("send delivery: %w", err)
	}
	return nil
}

func deliveryPlatformKey(accountID, platform string) string {
	return strings.TrimSpace(accountID) + "\x00" + strings.TrimSpace(platform)
}

// cronSchedule is a trigger-only schedule description. It deliberately has
// neither a command nor a payload: both remain owned by ZhiYuan.
type cronSchedule struct {
	Kind     string `json:"kind"`
	At       string `json:"at,omitempty"`
	EveryMs  int64  `json:"everyMs,omitempty"`
	AnchorMs *int64 `json:"anchorMs,omitempty"`
	Expr     string `json:"expr,omitempty"`
	Timezone string `json:"tz,omitempty"`
}

func newCronController(project string, bridge *bridgeClient, dataDirs ...string) *cronController {
	controller := &cronController{
		project: project,
		bridge:  bridge,
		jobs:    make(map[string]func()),
		pending: make(map[string]pendingTrigger),
		wake:    make(chan struct{}, 1),
		stopCh:  make(chan struct{}),
		done:    make(chan struct{}),
	}
	if len(dataDirs) > 0 && strings.TrimSpace(dataDirs[0]) != "" {
		controller.outboxPath = filepath.Join(dataDirs[0], "zhiyuan-trigger-outbox")
	}
	return controller
}

func (c *cronController) start() {
	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return
	}
	c.started = true
	_ = c.loadOutboxLocked()
	c.mu.Unlock()
	go c.deliverLoop()
	c.signalDelivery()
}

func (c *cronController) stop() context.Context {
	c.mu.Lock()
	if !c.started {
		c.mu.Unlock()
		return context.Background()
	}
	c.started = false
	close(c.stopCh)
	c.mu.Unlock()
	<-c.done
	return context.Background()
}

func (c *cronController) upsert(request cronTaskRequest) error {
	if strings.TrimSpace(request.TaskID) == "" || strings.TrimSpace(request.ScheduleVersion) == "" {
		return errors.New("taskId and scheduleVersion are required")
	}
	stop, err := c.register(request)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.jobs[request.TaskID]; ok {
		existing()
	}
	c.jobs[request.TaskID] = stop
	return nil
}

func (c *cronController) remove(taskID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	stop, ok := c.jobs[taskID]
	if !ok {
		return false
	}
	stop()
	delete(c.jobs, taskID)
	return true
}

func triggerIdentity(trigger pendingTrigger) string {
	return trigger.TaskID + "\x00" + trigger.ScheduleVersion + "\x00" + trigger.ScheduledAt.UTC().Format(time.RFC3339Nano)
}

func (c *cronController) trigger(taskID, version string, scheduledAt time.Time) {
	trigger := pendingTrigger{TaskID: taskID, ScheduleVersion: version, ScheduledAt: scheduledAt.UTC()}
	c.mu.Lock()
	identity := triggerIdentity(trigger)
	if err := c.persistTriggerLocked(identity, trigger); err != nil {
		fmt.Printf("persist cron trigger failed task_id=%s: %v\n", taskID, err)
	}
	c.pending[identity] = trigger
	c.mu.Unlock()
	c.signalDelivery()
}

func (c *cronController) register(request cronTaskRequest) (func(), error) {
	switch request.Schedule.Kind {
	case "cron":
		if strings.TrimSpace(request.Schedule.Expr) == "" {
			return nil, errors.New("cron expr is required")
		}
		location := time.Local
		if strings.TrimSpace(request.Schedule.Timezone) != "" {
			resolved, err := time.LoadLocation(request.Schedule.Timezone)
			if err != nil {
				return nil, fmt.Errorf("invalid timezone: %w", err)
			}
			location = resolved
		}
		schedule, err := cron.ParseStandard("CRON_TZ=" + location.String() + " " + request.Schedule.Expr)
		if err != nil {
			return nil, fmt.Errorf("invalid cron expression: %w", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			for {
				next := schedule.Next(time.Now())
				timer := time.NewTimer(time.Until(next))
				select {
				case <-ctx.Done():
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					return
				case <-timer.C:
					// A monotonic timer that expires while the machine sleeps is
					// delivered once on wake, intentionally recovering that missed
					// cron occurrence before calculating the next one.
					c.trigger(request.TaskID, request.ScheduleVersion, next)
				}
			}
		}()
		return cancel, nil
	case "at":
		at, err := time.Parse(time.RFC3339, request.Schedule.At)
		if err != nil {
			return nil, errors.New("at must be RFC3339")
		}
		if !at.After(time.Now()) {
			c.trigger(request.TaskID, request.ScheduleVersion, at)
			return func() {}, nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			timer := time.NewTimer(time.Until(at))
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				c.trigger(request.TaskID, request.ScheduleVersion, at)
			}
		}()
		return cancel, nil
	case "every":
		if request.Schedule.EveryMs <= 0 {
			return nil, errors.New("everyMs must be positive")
		}
		interval := time.Duration(request.Schedule.EveryMs) * time.Millisecond
		next := time.Now().Add(interval)
		if request.Schedule.AnchorMs != nil {
			anchor := time.UnixMilli(*request.Schedule.AnchorMs)
			for !anchor.After(time.Now()) {
				anchor = anchor.Add(interval)
			}
			next = anchor
		}
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			timer := time.NewTimer(time.Until(next))
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				c.trigger(request.TaskID, request.ScheduleVersion, next)
			}
			for {
				next = next.Add(interval)
				for !next.After(time.Now()) {
					next = next.Add(interval)
				}
				timer := time.NewTimer(time.Until(next))
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
					c.trigger(request.TaskID, request.ScheduleVersion, next)
				}
			}
		}()
		return cancel, nil
	default:
		return nil, errors.New("schedule.kind must be at, every, or cron")
	}
}

func (b *bridgeClient) triggerCron(
	ctx context.Context,
	project, taskID, scheduleVersion string,
	scheduledAt time.Time,
) error {
	requestID, err := newRequestID()
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{
		"requestId":       requestID,
		"project":         project,
		"taskId":          taskID,
		"scheduleVersion": scheduleVersion,
		"scheduledAt":     scheduledAt.Format(time.RFC3339Nano),
	})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint("/v1/cc-connect/cron/trigger"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+b.token)
	request.Header.Set("Content-Type", "application/json")
	if err := addProtocolHeaders(request, requestID); err != nil {
		return err
	}
	response, err := b.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("ZhiYuan cron bridge returned HTTP %d", response.StatusCode)
	}
	return nil
}

func startCronControlServer(ctx context.Context, rawAddress, token string, controllers *cronControllerRegistry, sender *deliverySender, statuses *platformStatusRegistry) (string, error) {
	address := strings.TrimSpace(rawAddress)
	host, _, err := net.SplitHostPort(address)
	if err != nil || !isLoopbackHost(host) {
		return "", errors.New("cron control address must be a loopback host:port")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(token) == "" {
		listener.Close()
		return "", errors.New("cron control token is required")
	}
	server := &http.Server{Handler: cronControlHandler(token, controllers, sender, statuses), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	go func() { _ = server.Serve(listener) }()
	return "http://" + listener.Addr().String(), nil
}

func (c *cronController) signalDelivery() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *cronController) deliverLoop() {
	defer close(c.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-c.wake:
		case <-ticker.C:
		}
		c.deliverPending()
	}
}

func (c *cronController) deliverPending() {
	c.mu.Lock()
	batch := make([]pendingTrigger, 0, len(c.pending))
	for _, trigger := range c.pending {
		batch = append(batch, trigger)
	}
	c.mu.Unlock()
	for _, trigger := range batch {
		ctx, cancel := context.WithTimeout(context.Background(), defaultTurnTimeout)
		err := c.bridge.triggerCron(ctx, c.project, trigger.TaskID, trigger.ScheduleVersion, trigger.ScheduledAt)
		cancel()
		if err != nil {
			fmt.Printf("cron trigger delivery failed task_id=%s: %v\n", trigger.TaskID, err)
			continue
		}
		c.mu.Lock()
		identity := triggerIdentity(trigger)
		if err := c.removePersistedTriggerLocked(identity); err != nil {
			fmt.Printf("persist cron trigger acknowledgment failed task_id=%s: %v\n", trigger.TaskID, err)
			c.mu.Unlock()
			continue
		}
		delete(c.pending, identity)
		c.mu.Unlock()
	}
}

func (c *cronController) loadOutboxLocked() error {
	if c.outboxPath == "" {
		return nil
	}
	entries, err := os.ReadDir(c.outboxPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(c.outboxPath, entry.Name()))
		if err != nil {
			return err
		}
		var trigger pendingTrigger
		if err := json.Unmarshal(data, &trigger); err != nil {
			return err
		}
		c.pending[triggerIdentity(trigger)] = trigger
	}
	return nil
}

func (c *cronController) persistTriggerLocked(identity string, trigger pendingTrigger) error {
	if c.outboxPath == "" {
		return nil
	}
	data, err := json.Marshal(trigger)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(c.outboxPath, 0o700); err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(identity))
	target := filepath.Join(c.outboxPath, fmt.Sprintf("%x.json", digest))
	if _, err := os.Stat(target); err == nil {
		return nil
	}
	temporary := target + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, target)
}

func (c *cronController) removePersistedTriggerLocked(identity string) error {
	if c.outboxPath == "" {
		return nil
	}
	digest := sha256.Sum256([]byte(identity))
	err := os.Remove(filepath.Join(c.outboxPath, fmt.Sprintf("%x.json", digest)))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func cronControlHandler(token string, controllers *cronControllerRegistry, sender *deliverySender, statuses *platformStatusRegistry) http.Handler {
	authenticator := &requestAuthenticator{token: token}
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if err := authenticator.authorize(request); err != nil {
			response.WriteHeader(http.StatusUnauthorized)
			return
		}
		const taskPath = "/v1/cc-connect/cron/tasks"
		const deliveryPath = "/v1/cc-connect/deliver"
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/v1/cc-connect/cron/health":
			response.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(response).Encode(currentHealth(statuses))
		case request.Method == http.MethodPost && request.URL.Path == taskPath:
			decoder := json.NewDecoder(io.LimitReader(request.Body, 64<<10))
			decoder.DisallowUnknownFields()
			var payload cronTaskRequest
			if err := decoder.Decode(&payload); err != nil || decoder.More() {
				http.Error(response, "invalid cron task request", http.StatusBadRequest)
				return
			}
			controller, err := controllers.resolve(payload.AccountID)
			if err != nil {
				http.Error(response, err.Error(), http.StatusBadRequest)
				return
			}
			if err := controller.upsert(payload); err != nil {
				http.Error(response, err.Error(), http.StatusBadRequest)
				return
			}
			response.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodPost && request.URL.Path == deliveryPath:
			if sender == nil {
				http.Error(response, "delivery is unavailable", http.StatusServiceUnavailable)
				return
			}
			decoder := json.NewDecoder(io.LimitReader(request.Body, 64<<10))
			decoder.DisallowUnknownFields()
			var payload deliveryRequest
			if err := decoder.Decode(&payload); err != nil || decoder.More() {
				http.Error(response, "invalid delivery request", http.StatusBadRequest)
				return
			}
			if err := sender.send(payload); err != nil {
				http.Error(response, err.Error(), http.StatusBadRequest)
				return
			}
			response.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodDelete && strings.HasPrefix(request.URL.Path, taskPath+"/"):
			taskID := strings.TrimPrefix(request.URL.Path, taskPath+"/")
			if taskID == "" || strings.Contains(taskID, "/") {
				http.Error(response, "invalid task id", http.StatusBadRequest)
				return
			}
			controller, err := controllers.resolve(request.URL.Query().Get("accountId"))
			if err != nil {
				http.Error(response, err.Error(), http.StatusBadRequest)
				return
			}
			if !controller.remove(taskID) {
				response.WriteHeader(http.StatusNotFound)
				return
			}
			response.WriteHeader(http.StatusNoContent)
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	})
}
