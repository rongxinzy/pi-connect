package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// cronController contains no durable task state and deliberately has no exec
// surface. ZhiYuan is the sole source of truth and reconciles registrations
// after any sidecar restart.
type cronController struct {
	project string
	bridge  *bridgeClient
	cron    *cron.Cron
	mu      sync.Mutex
	jobs    map[string]cron.EntryID
}

type cronTaskRequest struct {
	TaskID          string `json:"taskId"`
	ScheduleVersion string `json:"scheduleVersion"`
	Expression      string `json:"expression"`
	Timezone        string `json:"timezone,omitempty"`
}

func newCronController(project string, bridge *bridgeClient) *cronController {
	return &cronController{
		project: project,
		bridge:  bridge,
		cron:    cron.New(),
		jobs:    make(map[string]cron.EntryID),
	}
}

func (c *cronController) start() { c.cron.Start() }

func (c *cronController) stop() context.Context { return c.cron.Stop() }

func (c *cronController) upsert(request cronTaskRequest) error {
	if strings.TrimSpace(request.TaskID) == "" || strings.TrimSpace(request.ScheduleVersion) == "" {
		return errors.New("taskId and scheduleVersion are required")
	}
	if strings.TrimSpace(request.Expression) == "" {
		return errors.New("expression is required")
	}
	location := time.Local
	if strings.TrimSpace(request.Timezone) != "" {
		resolved, err := time.LoadLocation(request.Timezone)
		if err != nil {
			return fmt.Errorf("invalid timezone: %w", err)
		}
		location = resolved
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.jobs[request.TaskID]; ok {
		c.cron.Remove(existing)
	}
	entryID, err := c.cron.AddFunc("CRON_TZ="+location.String()+" "+request.Expression, func() {
		ctx, cancel := context.WithTimeout(context.Background(), defaultTurnTimeout)
		defer cancel()
		if err := c.bridge.triggerCron(ctx, c.project, request.TaskID, request.ScheduleVersion, time.Now().UTC()); err != nil {
			// The next ZhiYuan reconciliation is responsible for retry/backoff.
			fmt.Printf("cron trigger failed task_id=%s: %v\n", request.TaskID, err)
		}
	})
	if err != nil {
		return fmt.Errorf("invalid cron expression: %w", err)
	}
	c.jobs[request.TaskID] = entryID
	return nil
}

func (c *cronController) remove(taskID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.jobs[taskID]
	if !ok {
		return false
	}
	c.cron.Remove(entry)
	delete(c.jobs, taskID)
	return true
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
	request.Header.Set("X-ZhiYuan-Request-ID", requestID)
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

func startCronControlServer(ctx context.Context, rawAddress, token string, controller *cronController) (string, error) {
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
	server := &http.Server{Handler: cronControlHandler(token, controller), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	go func() { _ = server.Serve(listener) }()
	return "http://" + listener.Addr().String(), nil
}

func cronControlHandler(token string, controller *cronController) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if !secureBearerMatch(request.Header.Get("Authorization"), token) {
			response.WriteHeader(http.StatusUnauthorized)
			return
		}
		const taskPath = "/v1/cc-connect/cron/tasks"
		switch {
		case request.Method == http.MethodPost && request.URL.Path == taskPath:
			decoder := json.NewDecoder(io.LimitReader(request.Body, 64<<10))
			decoder.DisallowUnknownFields()
			var payload cronTaskRequest
			if err := decoder.Decode(&payload); err != nil || decoder.More() {
				http.Error(response, "invalid cron task request", http.StatusBadRequest)
				return
			}
			if err := controller.upsert(payload); err != nil {
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

func secureBearerMatch(value, token string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	provided := []byte(strings.TrimPrefix(value, prefix))
	expected := []byte(token)
	return len(provided) == len(expected) && subtle.ConstantTimeCompare(provided, expected) == 1
}
