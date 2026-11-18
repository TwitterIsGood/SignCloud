#!/bin/sh
set -eu

agent="$HOME/Library/Application Support/SignCloud/bin/signcloud-agent"
printf '%s SignCloud agent wrapper starting pid=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$$" >> "$HOME/Library/Logs/signcloud-agent-bootstrap.log"
exec "$agent" "$@"
