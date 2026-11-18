#!/bin/sh
set -eu

: "${SIGNCLOUD_API_TOKEN:?set SIGNCLOUD_API_TOKEN}"
server="${SIGNCLOUD_SERVER:-http://localhost:18787}"
source_file="${1:?usage: submit-example.sh source.zip [name]}"
name="${2:-Example iOS Build}"

curl --fail-with-body \
  -H "Authorization: Bearer $SIGNCLOUD_API_TOKEN" \
  -F "name=$name" \
  -F "kind=xcode" \
  -F 'spec={"export_method":"development","allow_provisioning_updates":true}' \
  -F "source=@$source_file" \
  "$server/v1/builds"
