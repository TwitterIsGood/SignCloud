#!/bin/sh
set -eu

cd "$(dirname "$0")/.."

if [ ! -f .env ]; then
  api_token="$(openssl rand -hex 32)"
  agent_token="$(openssl rand -hex 32)"
  db_password="$(openssl rand -hex 24)"
  lan_ip="$(ipconfig getifaddr en0 2>/dev/null || ipconfig getifaddr en1 2>/dev/null || printf 'localhost')"
  sed \
    -e "s/replace-with-a-long-random-token/$api_token/" \
    -e "s/replace-with-a-different-long-random-token/$agent_token/" \
    -e "s/replace-with-a-long-database-password/$db_password/g" \
    -e "s#https://build.example.com#http://$lan_ip:18787#" \
    .env.example > .env
  chmod 600 .env
  printf 'Created .env with random local credentials.\n'
fi

case "$(uname -m)" in
  arm64|aarch64) goarch=arm64 ;;
  x86_64|amd64) goarch=amd64 ;;
  *) printf 'Unsupported Docker host architecture: %s\n' "$(uname -m)" >&2; exit 1 ;;
esac
CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" go build -trimpath -ldflags='-s -w' -o bin/signcloud-server-linux ./cmd/server

docker compose up -d --build
printf 'SignCloud is starting at http://localhost:18787 (LAN: http://%s:18787)\n' "$(ipconfig getifaddr en0 2>/dev/null || ipconfig getifaddr en1 2>/dev/null || printf 'localhost')"
