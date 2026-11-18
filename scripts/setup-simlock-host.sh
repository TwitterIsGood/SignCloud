#!/bin/bash
# Idempotent setup for the SignCloud cloud-simulator device plane on macOS.
# Installs pinned simlock + agent-device, the simlock gateway/worker LaunchAgents,
# and creates gateway tokens once (secrets are written to a 0600 env file).
set -euo pipefail

SIMLOCK_VERSION="${SIMLOCK_VERSION:-0.5.0}"
AGENT_DEVICE_VERSION="${AGENT_DEVICE_VERSION:-0.21.36}"
GATEWAY_PORT="${GATEWAY_PORT:-4700}"
# The control plane runs in Docker, so the gateway cannot listen on loopback
# only: a container reaches the host through the LAN address. Keep the port
# private to the LAN and never forward it from the router.
default_lan_ip() {
  local iface
  iface="$(route -n get default 2>/dev/null | awk '/interface:/{print $2}')"
  [[ -n "$iface" ]] || return 0
  ipconfig getifaddr "$iface" 2>/dev/null || true
}
GATEWAY_HOST="${SIMLOCK_GATEWAY_HOST:-$(default_lan_ip)}"
if [[ -z "$GATEWAY_HOST" ]]; then
  GATEWAY_HOST=127.0.0.1
fi
TOKEN_FILE="${TOKEN_FILE:-$HOME/.simlock-gateway-tokens.env}"
WORKER_LABEL="${WORKER_LABEL:-$(hostname -s)}"
# macOS 27 hosts need an Xcode whose simulator service supports the host OS.
# Leave empty to keep the plist value that is already installed, or the
# xcode-select default. Example: /Applications/Xcode-27.1.0.app/Contents/Developer
DEVELOPER_DIR="${SIMLOCK_DEVELOPER_DIR:-}"

if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "setup-simlock-host.sh only runs on macOS" >&2
  exit 1
fi
if ! command -v npm >/dev/null 2>&1; then
  echo "npm is required (Node 22.13 or newer)" >&2
  exit 1
fi

echo "== Installing pinned CLIs"
installed_simlock="$(simlock --version 2>/dev/null || true)"
installed_agent="$(agent-device --version 2>/dev/null || true)"
if [[ "$installed_simlock" != "$SIMLOCK_VERSION" || "$installed_agent" != "$AGENT_DEVICE_VERSION" ]]; then
  npm install -g "simlock@$SIMLOCK_VERSION" "agent-device@$AGENT_DEVICE_VERSION"
else
  echo "simlock $SIMLOCK_VERSION and agent-device $AGENT_DEVICE_VERSION already installed"
fi

AGENT_DEVICE_BIN="$(command -v agent-device)"
echo "agent-device: $AGENT_DEVICE_BIN"

echo "== Configuring gateway"
simlock --gateway config set http.host "$GATEWAY_HOST"
simlock --gateway config set http.port "$GATEWAY_PORT"
simlock --gateway config set capabilities.agent-device.enabled true
simlock --gateway config set capabilities.stream.enabled true
simlock --gateway config set capabilities.input.enabled true
simlock --gateway daemon install
simlock --gateway daemon start >/dev/null 2>&1 || true

echo "== Preparing gateway tokens"
touch "$TOKEN_FILE"
chmod 600 "$TOKEN_FILE"
get_token() {
  local role="$1"
  local value
  value="$(grep -E "^${role}=" "$TOKEN_FILE" | head -1 | cut -d= -f2- || true)"
  printf '%s' "$value"
}
create_token() {
  local role="$1" label="$2"
  local output secret
  output="$(simlock --gateway token create --role "$role" --label "$label")"
  secret="$(printf '%s' "$output" | python3 -c 'import json,sys; print(json.load(sys.stdin)["secret"])')"
  if [[ -z "$secret" ]]; then
    echo "failed to create $role token" >&2
    exit 1
  fi
  printf '%s=%s\n' "$role" "$secret" >>"$TOKEN_FILE"
  printf '%s' "$secret"
}
WORKER_TOKEN="$(get_token worker)"
AGENT_TOKEN="$(get_token agent)"
OPERATOR_TOKEN="$(get_token operator)"
if [[ -z "$WORKER_TOKEN" ]]; then
  echo "creating worker token"
  WORKER_TOKEN="$(create_token worker "$WORKER_LABEL")"
fi
if [[ -z "$AGENT_TOKEN" ]]; then
  echo "creating agent token"
  AGENT_TOKEN="$(create_token agent "signcloud-agent-$WORKER_LABEL")"
fi
if [[ -z "$OPERATOR_TOKEN" ]]; then
  echo "creating operator token"
  OPERATOR_TOKEN="$(create_token operator "signcloud-control")"
fi

# Preserve an existing DEVELOPER_DIR override, because re-installing the worker
# regenerates its LaunchAgent plist.
WORKER_PLIST="$HOME/Library/LaunchAgents/com.callstack.simlock.worker.plist"
if [[ -z "$DEVELOPER_DIR" && -f "$WORKER_PLIST" ]]; then
  DEVELOPER_DIR="$(/usr/libexec/PlistBuddy -c 'Print :EnvironmentVariables:DEVELOPER_DIR' "$WORKER_PLIST" 2>/dev/null || true)"
fi

echo "== Configuring worker"
# http.enabled must be false before the worker joins a gateway.
simlock --local config set http.enabled false
simlock --local config set ios.defaultMode slim
simlock --local config set warmPool '{"enabled":false}'
simlock --local config set capacity '{"strategy":"fixed","config":{"maxRunning":2,"ios":{"maxDevices":2,"maxRunning":2}}}'
simlock --local config set capabilities.agent-device.enabled true
simlock --local config set capabilities.agent-device.command "$AGENT_DEVICE_BIN"
simlock --local config set capabilities.stream.enabled true
simlock --local config set capabilities.input.enabled true
simlock --local config set gateway "{\"url\":\"ws://$GATEWAY_HOST:$GATEWAY_PORT\",\"token\":\"$WORKER_TOKEN\",\"label\":\"$WORKER_LABEL\"}"
simlock --local daemon install
simlock --local daemon start >/dev/null 2>&1 || true

if [[ -n "$DEVELOPER_DIR" ]]; then
  echo "== Pinning worker DEVELOPER_DIR=$DEVELOPER_DIR"
  /usr/libexec/PlistBuddy -c 'Print :EnvironmentVariables' "$WORKER_PLIST" >/dev/null 2>&1 \
    || /usr/libexec/PlistBuddy -c 'Add :EnvironmentVariables dict' "$WORKER_PLIST"
  /usr/libexec/PlistBuddy -c 'Delete :EnvironmentVariables:DEVELOPER_DIR' "$WORKER_PLIST" >/dev/null 2>&1 || true
  /usr/libexec/PlistBuddy -c "Add :EnvironmentVariables:DEVELOPER_DIR string $DEVELOPER_DIR" "$WORKER_PLIST"
  launchctl bootout "gui/$(id -u)" "$WORKER_PLIST" >/dev/null 2>&1 || true
  sleep 1
  launchctl bootstrap "gui/$(id -u)" "$WORKER_PLIST" || { sleep 2; launchctl bootstrap "gui/$(id -u)" "$WORKER_PLIST"; }
fi

echo "== Checking health"
for _ in 1 2 3 4 5 6 7 8 9 10; do
  if curl -sS -f "http://$GATEWAY_HOST:$GATEWAY_PORT/v1/healthz" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
curl -sS "http://$GATEWAY_HOST:$GATEWAY_PORT/v1/healthz" || true
echo
simlock --gateway worker list || true

cat <<EOF

Setup complete. Next steps:
1. Worker environment (~/.signcloud-agent.env or deploy/agent.env.example):
   SIGNCLOUD_AGENT_DEVICE_URL=http://$GATEWAY_HOST:$GATEWAY_PORT/v1/agent-device
   SIGNCLOUD_AGENT_DEVICE_TOKEN=$AGENT_TOKEN
   SIGNCLOUD_AGENT_DEVICE_BIN=$AGENT_DEVICE_BIN
   SIGNCLOUD_SIMLOCK_GATEWAY_URL=http://$GATEWAY_HOST:$GATEWAY_PORT

2. Control plane .env:
   SIMLOCK_GATEWAY_URL=http://$GATEWAY_HOST:$GATEWAY_PORT
   SIMLOCK_OPERATOR_TOKEN=$OPERATOR_TOKEN
   NO_PROXY=localhost,127.0.0.1,host.docker.internal,$GATEWAY_HOST
   no_proxy=localhost,127.0.0.1,host.docker.internal,$GATEWAY_HOST
   The gateway host must be in NO_PROXY; otherwise a globally injected Docker
   proxy sends gateway requests through Clash and they time out.

3. The gateway listens on $GATEWAY_HOST so the control container can reach it.
   Keep that port on the LAN only and never forward it from the router: an
   agent token is effectively shell access to this Mac.

Tokens are stored in $TOKEN_FILE (mode 600); they are not printed again.
EOF
