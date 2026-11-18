# Simulator builds and verification

## Build to the caller's local Simulator

Requirements on the calling computer: macOS, Xcode, and at least one available iOS Simulator
runtime.

```bash
mkdir -p "$HOME/.signcloud"
curl -fsSL https://build.example.com/client/signcloud_ai.py -o "$HOME/.signcloud/signcloud_ai.py"
chmod +x "$HOME/.signcloud/signcloud_ai.py"

export SIGNCLOUD_SERVER=https://build.example.com
export SIGNCLOUD_API_TOKEN='provided separately'

python3 "$HOME/.signcloud/signcloud_ai.py" . \
  --kind simulator \
  --scheme MyApp \
  --cache-key company-myapp \
  --name MyApp-Simulator \
  --install-simulator
```

The first run uploads all unique blobs and builds cold; later runs upload only changed blobs. The
worker keeps the project at a stable path, preserves unchanged mtimes, and reuses DerivedData and
the Xcode Compilation Cache. The client waits, downloads the `.app.zip`, opens Simulator when
needed, runs `xcrun simctl install booted`, and launches the bundle.

## Cloud verification (`kind=verify`)

`verify` compiles the same path as `simulator`, then leases a cloud iOS Simulator through the
SignCloud agent. Install, launch or flow failures fail the job, but the evidence ZIP is still
uploaded and downloadable.

```bash
curl --fail-with-body \
  -H "Authorization: Bearer $SIGNCLOUD_API_TOKEN" \
  -F 'name=MyApp-Smoke' \
  -F 'kind=verify' \
  -F 'spec={"scheme":"MyApp","device":"iPhone 16","device_os":">=26","device_mode":"slim","verify_script":"SmokeTests/smoke.yaml"}' \
  -F 'source=@project.zip' \
  https://build.example.com/v1/builds
```

Verification `spec` fields:

- `device`: Simulator model, default `iPhone 16`.
- `device_os`: version or range such as `26.5`, `>=26`, `18 - 26`.
- `device_mode`: `slim` (default; drops push, StoreKit, iCloud and other background services) or
  `full`. The simlock worker's default mode ultimately decides; the response reports the leased mode.
- `verify_script`: `.ad` or `.yaml`/`.yml` path relative to the project root. A failing `wait` or
  assertion fails the job.
- `record_video`: capture a screen recording into the evidence ZIP (best effort).
- `verify_timeout_seconds`: 60–7200, default 900.

Flow results appear in `metrics.verification`:

```json
{"verdict":"failed","device":"iPhone 16","os":"26.5","mode":"slim",
 "failed_step":"verify script SmokeTests/smoke.yaml","duration_ms":18420}
```

Client equivalent:

```bash
python3 signcloud_ai.py . --kind verify --scheme MyApp \
  --device "iPhone 16" --device-os ">=26" --verify-script SmokeTests/smoke.yaml
```

While the device is leased, `GET /v1/builds/{id}` returns `view_url` (`/view/{install_token}`).
Open it to watch the device and tap or type into it; the browser never receives a simlock token.
The live view exists only while the job runs. Afterwards the page shows the evidence download link.
