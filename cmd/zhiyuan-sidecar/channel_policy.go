package main

import (
	"strings"

	"github.com/chenhg5/cc-connect/core"
)

const (
	chatTypeDirect = "direct"
	chatTypeGroup  = "group"
	policyOpen     = "open"
	policyPairing  = "pairing"
	policyAllow    = "allowlist"
	policyDisabled = "disabled"
)

type channelPolicy struct {
	dmPolicy       string
	dmAllowFrom    string
	groupPolicy    string
	groupAllowFrom string
}

func channelPolicyFromOptions(options map[string]any) channelPolicy {
	legacy, _ := options["allow_from"].(string)
	return channelPolicy{
		dmPolicy:       optionString(options, "dm_policy", policyOpen),
		dmAllowFrom:    optionString(options, "dm_allow_from", legacy),
		groupPolicy:    optionString(options, "group_policy", policyOpen),
		groupAllowFrom: optionString(options, "group_allow_from", legacy),
	}
}

func optionString(options map[string]any, key, fallback string) string {
	value, _ := options[key].(string)
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}

func (p channelPolicy) permits(msg *core.Message) bool {
	if msg == nil {
		return false
	}
	if channelChatType(msg) == chatTypeGroup {
		return policyPermits(p.groupPolicy, p.groupAllowFrom, bridgeChannelID(msg))
	}
	return policyPermits(p.dmPolicy, p.dmAllowFrom, msg.UserID)
}

func policyPermits(policy, allowFrom, identity string) bool {
	switch strings.ToLower(strings.TrimSpace(policy)) {
	case policyOpen:
		return true
	case policyDisabled:
		return false
	case policyPairing, policyAllow:
		return strings.TrimSpace(allowFrom) != "" && core.AllowList(allowFrom, identity)
	default:
		return false
	}
}

func channelChatType(msg *core.Message) string {
	if msg == nil {
		return chatTypeDirect
	}
	if msg.ChatType == chatTypeDirect || msg.ChatType == chatTypeGroup {
		return msg.ChatType
	}
	parts := strings.Split(msg.SessionKey, ":")
	if len(parts) >= 2 {
		switch msg.Platform {
		case "dingtalk", "qqbot":
			if parts[1] == "g" || len(parts) >= 3 && parts[1] != "d" && !strings.EqualFold(parts[1], msg.UserID) {
				return chatTypeGroup
			}
		case "telegram", "feishu", "lark":
			if len(parts) >= 3 && !strings.EqualFold(parts[1], msg.UserID) {
				return chatTypeGroup
			}
		case "wecom":
			if msg.ChatName != "" {
				return chatTypeGroup
			}
			return chatTypeDirect
		}
	}
	if msg.ChatName != "" && msg.Platform != "weixin" {
		return chatTypeGroup
	}
	return chatTypeDirect
}
