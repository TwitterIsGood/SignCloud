# SignCloud

SignCloud is a self-hosted iOS and macOS build, signing, and distribution service. Its
control plane runs in OrbStack/Docker. One or more macOS agents perform Xcode,
Expo, and IPA resigning jobs and upload the resulting IPA to an HTTPS install
page with a QR code and Apple OTA manifest, or a verified macOS DMG/app.zip.

## What the MVP supports

- Upload a zipped Xcode project or workspace and export a signed IPA.
- Build a Native iOS project for `iphonesimulator`, return a zipped `.app`,
  and install it on a developer Mac with `simctl`.
- Upload a zipped Expo/React Native project, run prebuild/CocoaPods, then export.
- Upload an existing IPA and resign it with a profile held by the macOS agent.
- Queue jobs across multiple macOS agents and stream command logs to the server.
- Public install page, QR code, OTA manifest, and direct IPA download.
- REST API usable from Windows, macOS, CI, or another internal service.
- AI-friendly wrapper that packages a project, submits it, polls status, and
  returns install/download URLs without exposing the token in prompts.
- AI-assisted physical-device enrollment: detect a paired iPhone locally,
  obtain user confirmation, register its UDID through the macOS worker, and
  prepare it for Ad Hoc distribution without exposing Apple credentials.
- Content-addressed project manifests and blob deduplication so repeat builds
  upload only changed files.
- Stable source paths, persistent project DerivedData, and Xcode Compilation
  Cache reuse for native incremental builds.
- A separate `macos` pipeline for native macOS projects, Universal binaries,
  ad-hoc/Developer ID signing, notarization, stapling, DMG, and optional app.zip.
- A standalone `resolve` job that runs Xcode package resolution on the worker
  and returns the rewritten `Package.resolved` files without building the app.
- A `verify` job that builds the Simulator app, leases a cloud iOS Simulator
  through [simlock](https://github.com/callstackincubator/simlock), installs and
  launches the app, runs a smoke check or an `.ad`/Maestro flow, and returns an
  evidence ZIP. A live browser view (`/view/{token}`) shows the device and
  accepts taps and keyboard input while the job runs.

The control plane never receives the signing certificate, private key,
provisioning profile, or App Store Connect API key. Those remain on the macOS
agent.

## Platform boundary

iOS compilation requires Xcode on macOS. OrbStack can host PostgreSQL and the
control plane, but it cannot replace a macOS/Xcode worker. A Windows developer
can submit a project, follow logs, download the result, and install through the
QR page. USB installation on Windows is a separate client capability requiring
Apple Mobile Device drivers plus a compatible installer such as
`ideviceinstaller`; it does not make Xcode available on Windows.

Development and Ad Hoc builds install only on devices included in the selected
provisioning profile. Use this service only for applications and devices your
organization is authorized to build and distribute. Do not expose unrestricted
third-party code execution on a general-purpose personal Mac.

## Start the control plane in OrbStack

```bash
cd /path/to/signcloud
chmod +x scripts/*.sh
./scripts/bootstrap.sh
```

The root URL returns JSON service metadata. SignCloud has no management panel;
all build management uses the authenticated API. The generated
`SIGNCLOUD_API_TOKEN` is stored in `.env`.

The only LAN port exposed by the default stack is TCP `18787`. PostgreSQL stays
inside the Docker network. When a public domain is ready, terminate HTTPS on
the router/reverse proxy and forward public TCP `443` to `MAC_LAN_IP:18787`.
Public TCP `80` is optional and is only needed for an HTTP-to-HTTPS redirect or
certificate challenge. Do not publish the PostgreSQL container.

For the current manually supplied wildcard certificate, the stack also exposes
local TCP `18443` as the HTTPS reverse-proxy entry. The router should forward
public TCP `8443` to `MAC_LAN_IP:18443`; do not forward it directly to `18787`.
Certificate and private-key copies live under the git-ignored `secrets/tls/`
directory. Renew them before the certificate expiry and rerun:

```bash
./scripts/install-tls.sh /path/to/certificate-directory
```

For public OTA installation, point a domain at this Mac/server, set these values
in `.env`, and start the HTTPS override:

```dotenv
SIGNCLOUD_DOMAIN=build.example.com
SIGNCLOUD_PUBLIC_BASE_URL=https://build.example.com
```

```bash
docker compose -f docker-compose.yml -f deploy/docker-compose.https.yml up -d --build
```

## Start a macOS build agent

Prerequisites:

- Xcode and Command Line Tools.
- A dedicated macOS user or disposable macOS VM.
- An encrypted signing P12 and its password file stored outside the repository.
- CocoaPods and Node for Expo jobs.
- A Development/Ad Hoc provisioning profile for resign jobs.
- For macOS distribution, a Developer ID Application identity in a dedicated
  Keychain and a worker-side `notarytool` Keychain profile.

Build the binaries:

```bash
make build
```

Inspect the local signing state before starting the agent:

```bash
./scripts/check-signing.sh
```

An identity may still appear in `security find-identity` even when its leaf
certificate has passed `Not After`; check the printed certificate dates as
well. Renew expired certificates before relying on resign jobs.

Configure the agent without placing secrets in the repository:

```bash
cp deploy/agent.env.example ~/.signcloud-agent.env
chmod 600 ~/.signcloud-agent.env
set -a
. ~/.signcloud-agent.env
set +a
./bin/signcloud-agent
```

For automatic signing, Xcode can use credentials already configured for that
macOS user. For unattended provisioning, set the optional App Store Connect API
key variables in the agent environment and enable **允许 Xcode 自动更新签名资源**
on the task.

## API

Create a build with multipart form data:

```bash
curl -H "Authorization: Bearer $SIGNCLOUD_API_TOKEN" \
  -F 'name=My iOS App' \
  -F 'kind=xcode' \
  -F 'spec={"scheme":"MyApp","export_method":"development","allow_provisioning_updates":true}' \
  -F 'source=@project.zip' \
  http://localhost:18787/v1/builds
```

Supported `kind` values are `xcode`, `expo`, `resign`, `simulator`,
`macos`, `resolve`, and `verify`.
`xcode` remains the iOS device IPA flow; `macos` is an independent desktop flow.
Useful iOS spec fields:

| Field | Purpose |
| --- | --- |
| `scheme` | Shared Xcode scheme; first scheme is used when omitted. |
| `workspace` | Relative `.xcworkspace` path when auto-discovery is ambiguous. |
| `project` | Relative `.xcodeproj` path when auto-discovery is ambiguous. |
| `configuration` | Defaults to `Release`. |
| `team_id` | Overrides `APPLE_TEAM_ID` for this task. |
| `export_method` | `development`, `ad-hoc`, `app-store`, or `enterprise`. Legacy names are mapped to Xcode 26's `debugging`, `release-testing`, and `app-store-connect`. |
| `allow_provisioning_updates` | Passes Xcode's provisioning update option. |
| `xcode_version` | Optional installed Xcode to build with, e.g. `26.6` or `27`. Omit to use the worker's `xcode-select` default. |

Multiple Xcodes can live side by side on a worker (for example
`/Applications/Xcode_26.6.app` and `/Applications/Xcode_27.app`). The worker
discovers `/Applications/Xcode*.app` by default; set `SIGNCLOUD_XCODE_PATHS`
(colon-separated, glob patterns allowed) to search other locations. A build
pins `DEVELOPER_DIR` to the selected Xcode, and build caches are keyed per
toolchain so switching versions never reuses stale results. `xcode_version`
accepts a major version (`27`), which resolves to the newest installed 27.x.

Submit a macOS Developer ID/notarized DMG with the official client:

```bash
python3 scripts/signcloud_ai.py PROJECT_PATH \
  --kind macos --scheme App --project App.xcodeproj \
  --configuration Release --team-id YOUR_TEAM_ID \
  --architectures arm64,x86_64 --signing developer-id \
  --notarize --artifact-format dmg --output ./App.dmg
```

The macOS spec is strictly validated: project/workspace are mutually exclusive,
the Scheme must be shared, and clients cannot supply a signing identity,
environment, prebuild shell, credentials, or arbitrary command. Worker settings
are documented in `deploy/agent.env.example`; increase
`MACOS_SIGNING_CONFIG_VERSION` whenever certificate or notarization state
changes so signed result caches are invalidated.

Register a paired iPhone before producing an Ad Hoc build for another user:

```bash
python3 scripts/signcloud_ai.py --list-local-devices
python3 scripts/signcloud_ai.py --enroll-device --device-index 1 --yes
python3 scripts/signcloud_ai.py PROJECT_PATH \
  --kind xcode --scheme AppScheme \
  --export-method ad-hoc --standard-export
```

For Ad Hoc builds, the app and extension bundle identifiers must be available
to the SignCloud Apple Developer team. Bundle identifiers owned by the source
project's original team cannot be registered by SignCloud. Use a working copy,
assign globally unique identifiers consistently across targets, enable
Automatic signing, and remove hard-coded provisioning profiles or signing
identities before submission.

The listing masks UDIDs. The enrollment command requires explicit confirmation
and the Apple API key remains on the macOS Agent. The control plane limits the
number of devices associated with each API token through
`SIGNCLOUD_MAX_DEVICES_PER_TOKEN`. Device enrollment is disabled until the
worker credentials are ready; enable it with
`SIGNCLOUD_DEVICE_ENROLLMENT_ENABLED=true`.

List and inspect builds:

```bash
curl -H "Authorization: Bearer $SIGNCLOUD_API_TOKEN" http://localhost:18787/v1/builds
curl -H "Authorization: Bearer $SIGNCLOUD_API_TOKEN" http://localhost:18787/v1/builds/BUILD_ID
```

## Use from an AI coding agent

Set `SIGNCLOUD_SERVER` and `SIGNCLOUD_API_TOKEN` on the developer's computer,
then let the agent run:

```bash
python3 scripts/signcloud_ai.py PROJECT_PATH --kind auto --name "App-1.0.0"
```

Directory projects use incremental transfer by default. Keep a stable cache key
across builds to reuse the same source tree and Xcode cache:

```bash
python3 scripts/signcloud_ai.py PROJECT_PATH \
  --kind simulator --scheme AppScheme --cache-key company-app-main \
  --install-simulator
```

Symbolic links are transferred and recreated on the worker, so versioned
framework layouts such as gomobile `.xcframework` bundles stay intact. The
client reports bytes hashed, uploaded, and reused. The completed build JSON
reports source-sync and build timing metrics. `--full-upload` selects the legacy
ZIP protocol. Eight or more missing blobs are compressed into one batch upload
to reduce HTTPS request overhead. Use `--clean` only when a clean Xcode build is
needed; normal builds preserve project DerivedData and Xcode's compilation
cache.

Development device builds use fast signed `.app` packaging by default, avoiding
Xcode Archive/Export. Add `--standard-export` for release workflows that require
the full export pipeline. Public artifacts support byte ranges and immutable
caching, and the AI client resumes interrupted downloads.

The platform-wide optimization requirements and acceptance criteria are defined
in [docs/PLATFORM_OPTIMIZATION.md](docs/PLATFORM_OPTIMIZATION.md). They are
internal engineering constraints, not caller-facing API instructions.

Build a Native iOS project in the cloud, then install and launch it on the
currently booted Simulator on the calling Mac:

```bash
python3 scripts/signcloud_ai.py PROJECT_PATH --kind simulator --scheme AppScheme --install-simulator
```

## Cloud simulator verification

`kind=verify` compiles the Simulator app on the worker, then drives a cloud iOS
Simulator on that same Mac. The device layer is
[simlock](https://github.com/callstackincubator/simlock) 0.5.0 (gateway +
worker launch agents) and
[agent-device](https://github.com/callstackincubator/agent-device) 0.21.36.

Host prerequisites (macOS 14+, Xcode installed, Node 22.13+):

```bash
scripts/setup-simlock-host.sh
```

The script installs pinned CLI versions, creates the gateway and worker
LaunchAgents, and prints the tokens once. It binds the gateway to the host's LAN
address by default (`SIMLOCK_GATEWAY_HOST` overrides it), because the control
plane is a container and cannot reach the host's loopback. Store the printed
values in the worker's environment (`deploy/agent.env.example`) and in the
control plane's `.env`:

```sh
# worker
SIGNCLOUD_AGENT_DEVICE_URL=http://192.0.2.20:4700/v1/agent-device
SIGNCLOUD_AGENT_DEVICE_TOKEN=<agent token>
SIGNCLOUD_AGENT_DEVICE_BIN=/Users/you/.nvm/versions/node/v22.19.0/bin/agent-device
SIGNCLOUD_SIMLOCK_GATEWAY_URL=http://192.0.2.20:4700

# control plane
SIMLOCK_GATEWAY_URL=http://192.0.2.20:4700
SIMLOCK_OPERATOR_TOKEN=<operator token>
NO_PROXY=localhost,127.0.0.1,host.docker.internal,192.0.2.20
```

The gateway host must also appear in `NO_PROXY`/`no_proxy`: a globally injected
Docker proxy otherwise routes gateway requests through the proxy and they time
out.

Submit a verification with the AI client:

```bash
python3 scripts/signcloud_ai.py PROJECT_PATH --kind verify --scheme AppScheme \
  --device "iPhone 16" --device-os ">=26" --verify-script SmokeTests/smoke.yaml
```

`verify_script` may be a native `.ad` flow or a supported Maestro YAML subset;
omit it for a default smoke check (wait, snapshot, screenshot). The job fails
when the flow fails, but the evidence ZIP — `report.json`, screenshots, command
log, optional video — is still uploaded and `artifact_url` remains available.
`metrics.verification` records `verdict`, device, OS, lease mode, duration, and
the failing step.

While the job runs, `GET /v1/builds/{id}` returns `view_url`. Opening it streams
the device into the browser (WebCodecs H.264) and forwards pointer and keyboard
input through the control plane; the browser never sees a simlock token. The
control plane proxies the stream with the operator token, so keep that token out
of client devices. The lease is released as soon as the job ends.

Limits worth knowing:

- `device_mode=slim` (default) disables push, StoreKit, iCloud, and similar
  background services to fit small hosts. Use `device_mode=full` only when the
  check needs them; the simlock worker must be configured with
  `ios.defaultMode=full` for full leases (the worker default decides the mode).
- iOS 27 runtimes cannot inject keystrokes; the viewer reports
  `keyboardUnavailable` and falls back to taps and swipes.
- Each viewer opens its own capture stream; several simultaneous viewers encode
  the screen more than once.
- The gateway listens on the LAN interface so the control container can reach
  it. Keep that port on the LAN only and never forward it from the router: an
  agent token is effectively shell access to the worker host.

The wrapper detects Expo/Xcode/IPA input, excludes common generated folders,
uploads with `curl`, polls the build, prints failure logs, and returns the
install and artifact URLs. See [AI_PROMPT.md](AI_PROMPT.md) for a copy-ready
Chinese prompt for Codex, Claude Code, Cursor, or another terminal-capable AI.
The latest wrapper is also available from `/client/signcloud_ai.py` so a remote
developer Mac can bootstrap without cloning this repository. A dedicated Native
iOS-to-local-Simulator prompt is in
[NATIVE_IOS_SIMULATOR_PROMPT.md](NATIVE_IOS_SIMULATOR_PROMPT.md).
Complete LLM-facing API and workflow documentation is publicly served from
`/llms.txt` following the proposed Markdown-oriented llms.txt convention.

Successful build responses include `install_url`, `qr_url`, `manifest_url`,
`ota_url`, and `artifact_url`. The install URL is the only small public web page
kept by the service; it exists solely so an iPhone user can scan and install a
specific build, not to manage the platform.

## Signing setup checklist

1. Record the 10-character Apple Team ID from Membership details.
2. Register test iPhone UDIDs in Certificates, Identifiers & Profiles.
3. Create an App ID matching the project's bundle identifier.
4. Create a Development or Ad Hoc provisioning profile containing the devices.
5. Store the profile and encrypted Apple Development/Distribution P12 on the
   dedicated macOS agent. The recommended ephemeral mode imports the P12 into
   a per-job keychain and deletes that keychain after every build.
6. Use a public HTTPS domain for QR/OTA installation.

## Security before external use

This repository is an operational MVP, not a multi-tenant sandbox. Before
allowing unrelated customers to upload code, add tenant authentication,
per-tenant quotas, audit logs, source retention policy, malware scanning,
network egress controls, and disposable macOS VM workers. Rotate both bearer
tokens if they are exposed. Never put `.p8`, `.p12`, or `.mobileprovision` files
inside the repository or control-plane container.
