#!/usr/bin/env bash
set -euo pipefail

# The ZhiYuan product sidecar is intentionally a Channel/Cron transport only.
# Keep this check next to its source so a future dependency change cannot
# silently link one of cc-connect's Agent/Provider runtimes back into the
# binary shipped by RongxinAI.
deps="$(go list -deps ./cmd/zhiyuan-sidecar)"

for forbidden in \
  'github.com/chenhg5/cc-connect/agent/' \
  'github.com/chenhg5/cc-connect/provider/' \
  'github.com/chenhg5/cc-connect/web/' \
  'github.com/chenhg5/cc-connect/cmd/cc-connect'; do
  if grep -Fq "$forbidden" <<<"$deps"; then
    echo "zhiyuan-sidecar must not link $forbidden" >&2
    exit 1
  fi
done

for required in \
  'github.com/chenhg5/cc-connect/core' \
  'github.com/chenhg5/cc-connect/platform/telegram' \
  'github.com/chenhg5/cc-connect/platform/discord' \
  'github.com/chenhg5/cc-connect/platform/dingtalk' \
  'github.com/chenhg5/cc-connect/platform/feishu' \
  'github.com/chenhg5/cc-connect/platform/qq' \
  'github.com/chenhg5/cc-connect/platform/wecom' \
  'github.com/chenhg5/cc-connect/platform/weixin'; do
  if ! grep -Fxq "$required" <<<"$deps"; then
    echo "zhiyuan-sidecar is missing required dependency $required" >&2
    exit 1
  fi
done

echo "zhiyuan-sidecar dependency boundary verified"
