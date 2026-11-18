#!/bin/sh
set -eu

tmp="$(mktemp /tmp/signcloud-certificates.XXXXXX.pem)"
trap 'unlink "$tmp"' EXIT
security find-certificate -a -c 'Apple Development' -p > "$tmp"

printf '%s\n' 'Apple Development certificates:'
openssl crl2pkcs7 -nocrl -certfile "$tmp" 2>/dev/null \
  | openssl pkcs7 -print_certs -text -noout 2>/dev/null \
  | awk '/Subject:/{subject=$0} /Not Before:/{before=$0} /Not After :/{print subject; print before; print $0; print ""}'

printf '%s\n' 'Code-signing identities:'
security find-identity -v -p codesigning

printf '%s\n' 'Device enrollment credentials:'
for key in APPLE_API_KEY_PATH APPLE_API_KEY_ID APPLE_API_ISSUER_ID APPLE_TEAM_ID; do
  eval "value=\${$key:-}"
  if [ -z "$value" ]; then
    printf '%s=missing\n' "$key"
  elif [ "$key" = APPLE_API_KEY_PATH ] && [ ! -f "$value" ]; then
    printf '%s=configured,file_missing\n' "$key"
  else
    printf '%s=configured\n' "$key"
  fi
done
if security find-identity -v -p codesigning | grep -Eq 'Apple Distribution|iPhone Distribution'; then
  printf '%s\n' 'Apple Distribution identity=available'
else
  printf '%s\n' 'Apple Distribution identity=missing'
fi

printf '%s\n' 'Provisioning profiles:'
count="$(find "$HOME/Library/MobileDevice/Provisioning Profiles" "$HOME/Library/Developer/Xcode/UserData/Provisioning Profiles" -maxdepth 1 -type f 2>/dev/null | wc -l | tr -d ' ')"
printf '%s profile(s) found\n' "$count"

printf '%s\n' 'Connected Apple devices:'
xcrun devicectl list devices 2>/dev/null || true
