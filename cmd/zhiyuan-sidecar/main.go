// zhiyuan-sidecar is a deliberately narrow cc-connect distribution.
// It owns native channel protocols only; ZhiYuan desktop owns every Agent turn.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/chenhg5/cc-connect/config"
	"github.com/chenhg5/cc-connect/core"
)

const (
	defaultTurnTimeout = 5 * time.Minute
	duplicateTTL       = 10 * time.Minute
)

type bridgeClient struct {
	endpoint string
	token    string
	client   *http.Client
}

func newBridgeClient(rawURL, token string) (*bridgeClient, error) {
	endpoint, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("parse bridge_url: %w", err)
	}
	if endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return nil, errors.New("bridge_url must use http or https")
	}
	if !isLoopbackHost(endpoint.Hostname()) {
		return nil, errors.New("bridge_url must target a loopback host")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("bridge_token is required")
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/v1/cc-connect/turn"
	return &bridgeClient{endpoint: endpoint.String(), token: token, client: &http.Client{Timeout: defaultTurnTimeout}}, nil
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (b *bridgeClient) runTurn(ctx context.Context, project string, msg *core.Message) (string, error) {
	requestID, err := newRequestID()
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(map[string]any{
		"requestId": requestID,
		"project":   project,
		"message": map[string]any{
			"sessionKey": msg.SessionKey, "platform": msg.Platform, "messageId": msg.MessageID,
			"channelId": msg.ChannelID, "userId": msg.UserID, "userName": msg.UserName,
			"chatName": msg.ChatName, "content": msg.Content, "extraContent": msg.ExtraContent,
			"images": msg.Images, "files": msg.Files, "userMessageTimeMs": msg.UserMessageTimeMs,
		},
	})
	if err != nil {
		return "", fmt.Errorf("encode bridge turn: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build bridge request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+b.token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-ZhiYuan-Request-ID", requestID)
	response, err := b.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("call ZhiYuan bridge: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ZhiYuan bridge returned HTTP %d", response.StatusCode)
	}
	var result struct{ Content string }
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&result); err != nil {
		return "", fmt.Errorf("decode bridge response: %w", err)
	}
	if strings.TrimSpace(result.Content) == "" {
		return "", errors.New("ZhiYuan bridge returned an empty response")
	}
	return result.Content, nil
}

func newRequestID() (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", fmt.Errorf("generate request ID: %w", err)
	}
	return hex.EncodeToString(data), nil
}

type deduplicator struct {
	mu      sync.Mutex
	entries map[string]time.Time
}

func (d *deduplicator) accept(key string, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.entries == nil {
		d.entries = make(map[string]time.Time)
	}
	for existing, expiresAt := range d.entries {
		if !expiresAt.After(now) {
			delete(d.entries, existing)
		}
	}
	if _, exists := d.entries[key]; exists {
		return false
	}
	d.entries[key] = now.Add(duplicateTTL)
	return true
}

func main() {
	configPath := os.Getenv("CC_CONNECT_CONFIG")
	if strings.TrimSpace(configPath) == "" {
		fmt.Fprintln(os.Stderr, "CC_CONNECT_CONFIG is required")
		os.Exit(2)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var platforms []core.Platform
	defer func() {
		for _, platform := range platforms {
			if err := platform.Stop(); err != nil {
				slog.Warn("stop platform", "platform", platform.Name(), "error", err)
			}
		}
	}()
	for _, project := range cfg.Projects {
		bridgeURL, _ := project.Agent.Options["bridge_url"].(string)
		bridgeToken, _ := project.Agent.Options["bridge_token"].(string)
		bridge, err := newBridgeClient(bridgeURL, bridgeToken)
		if err != nil {
			fmt.Fprintf(os.Stderr, "project %q bridge configuration: %v\n", project.Name, err)
			os.Exit(2)
		}
		for _, platformConfig := range project.Platforms {
			options := make(map[string]any, len(platformConfig.Options)+2)
			for key, value := range platformConfig.Options {
				options[key] = value
			}
			options["cc_data_dir"], options["cc_project"] = cfg.DataDir, project.Name
			platform, err := core.CreatePlatform(platformConfig.Type, options)
			if err != nil {
				fmt.Fprintf(os.Stderr, "project %q platform %q: %v\n", project.Name, platformConfig.Type, err)
				os.Exit(2)
			}
			dedup := &deduplicator{}
			if err := platform.Start(func(p core.Platform, msg *core.Message) {
				if msg == nil || strings.TrimSpace(msg.MessageID) == "" || !dedup.accept(project.Name+":"+msg.Platform+":"+msg.MessageID, time.Now()) {
					return
				}
				go func() {
					turnCtx, turnCancel := context.WithTimeout(ctx, defaultTurnTimeout)
					defer turnCancel()
					content, err := bridge.runTurn(turnCtx, project.Name, msg)
					if err != nil {
						slog.Error("ZhiYuan bridge turn failed", "project", project.Name, "platform", p.Name(), "message_id", msg.MessageID, "error", err)
						_ = p.Reply(context.Background(), msg.ReplyCtx, "暂时无法处理这条消息，请稍后重试。")
						return
					}
					if err := p.Reply(context.Background(), msg.ReplyCtx, content); err != nil {
						slog.Error("send channel reply", "project", project.Name, "platform", p.Name(), "message_id", msg.MessageID, "error", err)
					}
				}()
			}); err != nil {
				fmt.Fprintf(os.Stderr, "start project %q platform %q: %v\n", project.Name, platform.Name(), err)
				os.Exit(2)
			}
			platforms = append(platforms, platform)
		}
	}
	<-ctx.Done()
}
