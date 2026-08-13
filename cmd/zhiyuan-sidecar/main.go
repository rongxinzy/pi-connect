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
	"syscall"
	"time"

	"github.com/chenhg5/cc-connect/config"
	"github.com/chenhg5/cc-connect/core"
)

const (
	defaultTurnTimeout = 5 * time.Minute
	defaultMediaMaxMB  = 100
)

type bridgeClient struct {
	baseURL *url.URL
	token   string
	client  *http.Client
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
	return &bridgeClient{baseURL: endpoint, token: token, client: &http.Client{Timeout: defaultTurnTimeout}}, nil
}

func (b *bridgeClient) endpoint(pathSuffix string) string {
	endpoint := *b.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + pathSuffix
	return endpoint.String()
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (b *bridgeClient) runTurn(ctx context.Context, accountID string, msg *core.Message) (bridgeTurnResponse, error) {
	requestID, err := newRequestID()
	if err != nil {
		return bridgeTurnResponse{}, err
	}
	body, err := json.Marshal(map[string]any{
		"requestId": requestID,
		"accountId": accountID,
		"message": map[string]any{
			"sessionKey": msg.SessionKey, "platform": msg.Platform, "messageId": msg.MessageID,
			"channelId": bridgeChannelID(msg), "userId": msg.UserID, "userName": msg.UserName,
			"chatName": msg.ChatName, "chatType": channelChatType(msg), "content": msg.Content, "extraContent": msg.ExtraContent,
			"images": msg.Images, "files": msg.Files, "audio": msg.Audio, "userMessageTimeMs": msg.UserMessageTimeMs,
		},
	})
	if err != nil {
		return bridgeTurnResponse{}, fmt.Errorf("encode bridge turn: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint("/v1/cc-connect/turn"), bytes.NewReader(body))
	if err != nil {
		return bridgeTurnResponse{}, fmt.Errorf("build bridge request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+b.token)
	request.Header.Set("Content-Type", "application/json")
	if err := addProtocolHeaders(request, requestID); err != nil {
		return bridgeTurnResponse{}, fmt.Errorf("secure bridge request: %w", err)
	}
	response, err := b.client.Do(request)
	if err != nil {
		return bridgeTurnResponse{}, fmt.Errorf("call ZhiYuan bridge: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return bridgeTurnResponse{}, fmt.Errorf("ZhiYuan bridge returned HTTP %d", response.StatusCode)
	}
	var result bridgeTurnResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&result); err != nil {
		return bridgeTurnResponse{}, fmt.Errorf("decode bridge response: %w", err)
	}
	if strings.TrimSpace(result.Content) == "" && len(result.Attachments) == 0 {
		return bridgeTurnResponse{}, errors.New("ZhiYuan bridge returned an empty response")
	}
	return result, nil
}

func bridgeChannelID(msg *core.Message) string {
	if msg == nil {
		return ""
	}
	if channelKey := strings.TrimSpace(msg.ChannelKey); channelKey != "" {
		return channelKey
	}
	return strings.TrimSpace(msg.ChannelID)
}

type configuredPlatform struct {
	accountID     string
	platform      core.Platform
	bridge        *bridgeClient
	isAsync       bool
	policy        channelPolicy
	mediaMaxBytes int64
}

func newRequestID() (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", fmt.Errorf("generate request ID: %w", err)
	}
	return hex.EncodeToString(data), nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "weixin-setup" {
		if err := runWeixinSetupCommand(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		return
	}
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
	var controllers []*cronController
	defer func() {
		for _, platform := range platforms {
			if err := platform.Stop(); err != nil {
				slog.Warn("stop platform", "platform", platform.Name(), "error", err)
			}
		}
		for _, controller := range controllers {
			controller.stop()
		}
	}()
	var controlListen string
	var controlToken string
	platformStatuses := newPlatformStatusRegistry()
	controlSender := &deliverySender{statuses: platformStatuses}
	var configuredPlatforms []configuredPlatform
	for _, project := range cfg.Projects {
		bridgeURL, _ := project.Agent.Options["bridge_url"].(string)
		bridgeToken, _ := project.Agent.Options["bridge_token"].(string)
		bridge, err := newBridgeClient(bridgeURL, bridgeToken)
		if err != nil {
			fmt.Fprintf(os.Stderr, "project %q bridge configuration: %v\n", project.Name, err)
			os.Exit(2)
		}
		cronControlListen, _ := project.Agent.Options["cron_control_listen"].(string)
		if strings.TrimSpace(cronControlListen) == "" {
			fmt.Fprintf(os.Stderr, "project %q bridge configuration: cron_control_listen is required\n", project.Name)
			os.Exit(2)
		}
		if controlListen == "" {
			controlListen, controlToken = cronControlListen, bridgeToken
		} else if controlListen != cronControlListen || controlToken != bridgeToken {
			fmt.Fprintln(os.Stderr, "all projects must share cron_control_listen and bridge_token")
			os.Exit(2)
		}
		cronController := newCronController(project.Name, bridge, cfg.DataDir)
		cronController.start()
		controllers = append(controllers, cronController)
		for _, platformConfig := range project.Platforms {
			options := make(map[string]any, len(platformConfig.Options)+2)
			for key, value := range platformConfig.Options {
				options[key] = value
			}
			options["cc_data_dir"], options["cc_project"] = cfg.DataDir, project.Name
			platform, err := core.CreatePlatform(platformConfig.Type, options)
			if err != nil {
				platformStatuses.set(project.Name, platformConfig.Type, platformStateUnavailable, err)
				slog.Error("create channel platform", "project", project.Name, "platform", platformConfig.Type, "error", err)
				continue
			}
			platformStatuses.set(project.Name, platform.Name(), platformStateStarting, nil)
			_, isAsync := platform.(core.AsyncRecoverablePlatform)
			if async, ok := platform.(core.AsyncRecoverablePlatform); ok {
				async.SetLifecycleHandler(&projectPlatformLifecycle{accountID: project.Name, statuses: platformStatuses})
			}
			configuredPlatforms = append(configuredPlatforms, configuredPlatform{
				accountID:     project.Name,
				platform:      platform,
				bridge:        bridge,
				isAsync:       isAsync,
				policy:        channelPolicyFromOptions(options),
				mediaMaxBytes: mediaMaxBytesFromOptions(options),
			})
			platforms = append(platforms, platform)
		}
	}
	if len(controllers) == 0 {
		fmt.Fprintln(os.Stderr, "at least one project is required")
		os.Exit(2)
	}
	controlURL, err := startCronControlServer(ctx, controlListen, controlToken, newCronControllerRegistry(controllers), controlSender, platformStatuses)
	if err != nil {
		fmt.Fprintf(os.Stderr, "channel control: %v\n", err)
		os.Exit(2)
	}
	slog.Info("channel control listening", "projects", len(controllers), "url", controlURL)
	for _, configured := range configuredPlatforms {
		configured := configured
		go func() {
			if err := configured.platform.Start(func(p core.Platform, msg *core.Message) {
				if msg == nil || strings.TrimSpace(msg.MessageID) == "" {
					return
				}
				if !configured.policy.permits(msg) {
					slog.Warn("channel message rejected by policy", "account_id", configured.accountID, "platform", p.Name(), "message_id", msg.MessageID)
					return
				}
				platformStatuses.markInbound(configured.accountID, p.Name())
				go func() {
					turnCtx, turnCancel := context.WithTimeout(ctx, defaultTurnTimeout)
					defer turnCancel()
					stopTyping := func() {}
					if typing, ok := p.(core.TypingIndicator); ok {
						stopTyping = typing.StartTyping(turnCtx, msg.ReplyCtx)
					}
					defer stopTyping()
					if err := validateInboundMedia(msg, configured.mediaMaxBytes); err != nil {
						slog.Warn("channel media rejected", "account_id", configured.accountID, "platform", p.Name(), "message_id", msg.MessageID, "error", err)
						return
					}
					result, err := configured.bridge.runTurn(turnCtx, configured.accountID, msg)
					if err != nil {
						slog.Error("ZhiYuan bridge turn failed", "account_id", configured.accountID, "platform", p.Name(), "message_id", msg.MessageID, "error", err)
						replyCtx, replyCancel := context.WithTimeout(context.Background(), 30*time.Second)
						defer replyCancel()
						if replyErr := p.Reply(replyCtx, msg.ReplyCtx, "暂时无法处理这条消息，请稍后重试。"); replyErr != nil {
							slog.Error("send channel fallback reply", "account_id", configured.accountID, "platform", p.Name(), "message_id", msg.MessageID, "error", replyErr)
						}
						return
					}
					replyCtx, replyCancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer replyCancel()
					if err := sendBridgeTurnResponse(replyCtx, p, msg.ReplyCtx, result); err != nil {
						slog.Error("send channel reply", "account_id", configured.accountID, "platform", p.Name(), "message_id", msg.MessageID, "error", err)
					} else {
						platformStatuses.markOutbound(configured.accountID, p.Name())
						slog.Info("channel reply sent", "account_id", configured.accountID, "platform", p.Name(), "message_id", msg.MessageID)
					}
				}()
			}); err != nil {
				platformStatuses.set(configured.accountID, configured.platform.Name(), platformStateUnavailable, err)
				slog.Error("start channel platform", "account_id", configured.accountID, "platform", configured.platform.Name(), "error", err)
				return
			}
			if !configured.isAsync {
				platformStatuses.set(configured.accountID, configured.platform.Name(), platformStateReady, nil)
			}
			controlSender.register(configured.accountID, configured.platform)
		}()
	}
	<-ctx.Done()
}
