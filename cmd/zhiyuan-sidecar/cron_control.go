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
	mu      sync.Mutex
	jobs    map[string]func()
}

type cronTaskRequest struct {
	TaskID          string       `json:"taskId"`
	ScheduleVersion string       `json:"scheduleVersion"`
	Schedule        cronSchedule `json:"schedule"`
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

func newCronController(project string, bridge *bridgeClient) *cronController {
	return &cronController{
		project: project,
		bridge:  bridge,
		jobs:    make(map[string]func()),
	}
}

func (c *cronController) start() {}

func (c *cronController) stop() context.Context { return context.Background() }

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

func (c *cronController) trigger(taskID, version string) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTurnTimeout)
	defer cancel()
	if err := c.bridge.triggerCron(ctx, c.project, taskID, version, time.Now().UTC()); err != nil {
		// ZhiYuan reconciliation is responsible for retry/backoff.
		fmt.Printf("cron trigger failed task_id=%s: %v\n", taskID, err)
	}
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
					c.trigger(request.TaskID, request.ScheduleVersion)
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
			return nil, errors.New("at schedule must be in the future")
		}
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			timer := time.NewTimer(time.Until(at))
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				c.trigger(request.TaskID, request.ScheduleVersion)
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
				c.trigger(request.TaskID, request.ScheduleVersion)
			}
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					c.trigger(request.TaskID, request.ScheduleVersion)
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
		case request.Method == http.MethodGet && request.URL.Path == "/v1/cc-connect/cron/health":
			response.WriteHeader(http.StatusNoContent)
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
