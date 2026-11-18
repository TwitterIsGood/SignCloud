#!/bin/sh
set -eu

source_dir="${1:?usage: install-tls.sh CERTIFICATE_DIRECTORY}"
project_dir="$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)"
cert="$source_dir/fullchain.pem"
key="$source_dir/privkey.pem"
if [ ! -f "$cert" ]; then
  cert="$(find "$source_dir" -maxdepth 1 -name '*.pem' -print | head -n 1)"
  key="$(find "$source_dir" -maxdepth 1 -name '*.key' -print | head -n 1)"
fi

test -f "$cert"
test -f "$key"

mkdir -p "$project_dir/secrets/tls"
install -m 644 "$cert" "$project_dir/secrets/tls/fullchain.pem"
install -m 600 "$key" "$project_dir/secrets/tls/privkey.pem"

cert_pub="$(openssl x509 -in "$cert" -pubkey -noout | openssl pkey -pubin -outform DER 2>/dev/null | shasum -a 256 | awk '{print $1}')"
key_pub="$(openssl pkey -in "$key" -pubout -outform DER 2>/dev/null | shasum -a 256 | awk '{print $1}')"
test "$cert_pub" = "$key_pub"

cd "$project_dir"
docker compose up -d --force-recreate tls
printf 'TLS certificate installed. Forward the public HTTPS port to this host TCP 18443.\n'
