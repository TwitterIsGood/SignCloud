# Real iPhone builds

The cloud worker compiles and signs; installation happens on the iPhone.

## Apple setup

1. Obtain the target iPhone UDID.
2. Register the UDID in the Apple Developer team.
3. Use an App ID matching the project's bundle identifier.
4. Install a valid Apple Development or Apple Distribution identity on the Mac worker.
5. Create or automatically obtain a Development/Ad Hoc profile containing the UDID.
6. Set the Apple Team ID on the worker or in `spec.team_id`.

Before submitting, inspect every iOS target's signing settings. A bundle identifier already
registered by another Apple team cannot be used. Change it to a globally unique identifier
available to the SignCloud team, update extension identifiers consistently, set signing to
Automatic, and remove hard-coded provisioning profiles or signing identities. Do not modify the
user's only copy without approval; use a working copy when the identifier change is only for
SignCloud.

## Enroll a connected iPhone

Do not ask the user to paste a full UDID into chat. On their Mac, list paired physical iPhones
(UDIDs are masked):

```bash
python3 "$HOME/.signcloud/signcloud_ai.py" --list-local-devices
```

Tell the user which device name, model and masked UDID will be sent, and get explicit confirmation.
Then enroll by the displayed 1-based index:

```bash
python3 "$HOME/.signcloud/signcloud_ai.py" --enroll-device --device-index 1 --yes
```

The client reads the full UDID locally from `xcrun devicectl`, submits it over the authenticated
API, and waits for the Mac agent to register it with Apple. The Apple API key stays on the worker.
Never request, print, upload or place an Apple `.p8`, private signing key or certificate password
in a prompt.

List devices associated with the current token:

```bash
python3 "$HOME/.signcloud/signcloud_ai.py" --list-registered-devices
```

## Build for a registered device

Development build (fast packaging is the client default):

```bash
python3 "$HOME/.signcloud/signcloud_ai.py" . \
  --kind xcode \
  --scheme MyApp \
  --configuration Debug \
  --cache-key company-myapp \
  --name MyApp-iPhone \
  --export-method development
```

Ad Hoc distribution to other people's devices — every target device must be in the profile used
for export, and an existing IPA does not gain access to a newly enrolled device:

```bash
python3 "$HOME/.signcloud/signcloud_ai.py" . \
  --kind xcode \
  --scheme MyApp \
  --configuration Release \
  --cache-key company-myapp \
  --name MyApp-AdHoc \
  --export-method ad-hoc \
  --standard-export
```

Add `--standard-export` to force Xcode Archive/Export. Keep the same `--cache-key` and avoid
`--clean` during normal edit/build/install cycles.

After `succeeded`, open `install_url` on the iPhone or scan `qr_url`. The install page loads
`ota_url`, which reads `manifest_url` and downloads the signed IPA from `artifact_url` over HTTPS.

## Device enrollment API

All user endpoints use the normal bearer token. Devices are isolated by API token and the server
applies a per-token quota. `POST /v1/devices` returns `503` until the administrator configures the
worker Apple API key and sets `SIGNCLOUD_DEVICE_ENROLLMENT_ENABLED=true`.

- `POST /v1/devices` with `{"udid","name","platform":"IOS"}` queues a registration task.
- `GET /v1/devices` lists devices for the current token.
- `GET /v1/devices/{id}` reports `queued`, `running`, `registered` or `failed`.

User responses contain `masked_udid`, never the full identifier. Only the authenticated Mac agent
receives the full UDID while processing the task. Enrollment consumes the team's annual device
quota, so never enroll speculatively or without confirmation.
