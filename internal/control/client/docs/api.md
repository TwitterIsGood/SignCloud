# SignCloud API and builds

Base URL: `https://build.example.com`

Every `/v1/*` request carries `Authorization: Bearer $SIGNCLOUD_API_TOKEN`. Read the token from the
caller's `SIGNCLOUD_API_TOKEN` environment variable; never invent one. Public artifact,
`/install/{token}`, `/manifest/{token}.plist` and `/view/{token}` URLs need no header.

## Build kinds

- `xcode`: native Swift/Objective-C iOS project archived for a real iPhone and exported as a signed `.ipa`.
- `simulator`: the same project compiled for `iphonesimulator`, Debug by default, no distribution
  signing. Returns a `*-simulator.zip` containing one `.app`; the calling Mac installs it into its
  own Simulator. Use `verify` when SignCloud should drive the app instead.
- `verify`: builds the `iphonesimulator` app, leases a cloud Simulator, installs, launches, runs an
  optional `.ad` or Maestro `.yaml` flow (or a default smoke check), and returns an evidence ZIP
  (`report.json`, screenshots, command log, optional video). Never needs signing credentials.
  See [Simulator verification](simulator.md).
- `expo`: Expo/React Native project; installs dependencies, runs Expo prebuild and CocoaPods, then
  archives a signed `.ipa`.
- `resign`: an existing `.ipa` re-signed by the worker.
- `macos`: desktop app for arm64, x86_64 or Universal, with ad-hoc or Developer ID signing, optional
  notarization, DMG and optional app.zip. See [macOS](macos.md).
- `resolve`: runs `xcodebuild -resolvePackageDependencies` and returns a ZIP of updated
  `Package.resolved` files instead of an app.

`kind=xcode` always means an iOS device IPA; `kind=macos` always means a desktop app. Do not submit
one as the other. `resolve` produces no artifact and no install page. `verify` has no install page;
it exposes `view_url` while running and `artifact_url` (evidence) on success and failure.

## Create a build

For directory projects prefer the content-addressed incremental protocol used by the official
client: compute a SHA-256 manifest, `POST /v1/uploads/plan`, upload missing content with
`PUT /v1/blobs/{sha256}` (eight or more missing blobs are combined into one compressed
`PUT /v1/blobs/batch`), then `POST /v1/builds/incremental`.

The legacy multipart endpoint remains for ZIP files and compatibility:

```bash
curl --fail-with-body \
  -H "Authorization: Bearer $SIGNCLOUD_API_TOKEN" \
  -F 'name=MyApp-Device' \
  -F 'kind=xcode' \
  -F 'spec={"scheme":"MyApp","export_method":"development","allow_provisioning_updates":true}' \
  -F 'source=@project.zip' \
  https://build.example.com/v1/builds
```

Multipart fields: `name` (display name), `kind`, `spec` (JSON object), `source` (`.zip`, or `.ipa`
for `resign`).

Useful `spec` fields:

```json
{
  "scheme": "MyApp",
  "workspace": "MyApp.xcworkspace",
  "project": "MyApp.xcodeproj",
  "configuration": "Debug",
  "team_id": "APPLE_TEAM_ID",
  "export_method": "development",
  "fast_package": true,
  "build_profile": "fast",
  "allow_provisioning_updates": true,
  "xcode_version": "27"
}
```

`workspace` or `project` is optional; the worker discovers the Xcode container when unambiguous.
`export_method` is `development`, `ad-hoc`, `app-store` or `enterprise`. For Development builds,
`fast_package: true` (the official client's default) packages the signed `.app` straight into an
IPA and skips Archive/Export. Use the standard path for Ad Hoc, App Store, Enterprise, or projects
whose export process performs custom transformations (`--standard-export` in the client).

`build_profile: "fast"` (the official client's default for `xcode`, `expo` and `simulator`)
applies only to Debug builds and leaves Release/Archive untouched. It turns off the index store
(`COMPILER_INDEX_STORE_ENABLE=NO`), which only Xcode's editor reads. For `simulator`, an added
`simulator_arch` (`arm64` or `x86_64`; the client sends its own Mac's architecture) builds that
architecture only instead of both. A setting the project or its `.xcconfig` files declare
explicitly is kept, as is `ARCHS` when the project excludes the requested architecture; the
build's `build_profile_settings` and `build_profile_notes` metrics say what was applied. Use
`--standard-build` in the client to keep every setting as declared.

`xcode_version` is accepted by every kind. Give a major version (`"26"`, `"27"`) for the newest
installed Xcode in that series or a full version (`"26.6"`) to pin it. Omit it for the worker's
`xcode-select` default. Malformed values return `400`; an uninstalled version fails with the list
of available versions. The Xcode version participates in build and result cache keys.

## Resolve Swift package dependencies

```bash
curl --fail-with-body \
  -H "Authorization: Bearer $SIGNCLOUD_API_TOKEN" \
  -F 'name=MyApp-Resolve' \
  -F 'kind=resolve' \
  -F 'spec={"project":"MyApp.xcodeproj"}' \
  -F 'source=@project.zip' \
  https://build.example.com/v1/builds
```

Poll until `succeeded`, then download `artifact_url`. Entries are `Package.resolved` files at their
project-relative paths, e.g.
`MyApp.xcodeproj/project.xcworkspace/xcshareddata/swiftpm/Package.resolved`. Resolution always
consults the remote package graph, so it repairs stale pins, checksums and package identities.
`spec.workspace` and `spec.project` are optional. Client equivalent:

```bash
python3 signcloud_ai.py . --kind resolve --project MyApp.xcodeproj
```

## Build response

- `id`, `kind`, `status` (`queued`, `running`, `succeeded`, `failed`, `canceled`), `claimed_by`.
- `logs`, `error`.
- `bundle_id`, `version`, `build_number`, `minimum_os`, `minimum_system_version`.
- `artifact_url`: IPA, Simulator ZIP, resolved-lock ZIP or verification evidence ZIP. Failed
  `verify` jobs still expose it.
- `verdict`, `view_url`: `verify` only.
- `install_url`, `qr_url`, `manifest_url`, `ota_url`: real-device builds only.
- macOS results add `stage`, `architectures`, `signing`, `signed`, `notarized`, `stapled`, `sha256`
  and optional `app_zip_url`.
- `metrics`: source sync, build strategy, result/dependency cache, Xcode, Archive, Export,
  packaging, CPU and peak memory. Failed builds keep metrics gathered before the failure.

## Poll, list, cancel

```bash
curl -H "Authorization: Bearer $SIGNCLOUD_API_TOKEN" https://build.example.com/v1/builds/BUILD_ID
curl -H "Authorization: Bearer $SIGNCLOUD_API_TOKEN" "https://build.example.com/v1/builds?status=running&limit=50"
curl -X POST -H "Authorization: Bearer $SIGNCLOUD_API_TOKEN" https://build.example.com/v1/builds/BUILD_ID/cancel
```

Poll every 5 seconds until a terminal status. A build stuck in `queued` means no worker claimed it.
On failure read `error` and the log tail, fix the project, submit a new build. `GET /v1/builds`
returns newest first; `limit` defaults to 200, maximum 1000. `cancel` returns `409` for builds that
already reached a terminal status.

## Limits

- Maximum upload: 2 GB.
- Client wait timeout: 2700 seconds by default.
- Parallel blob uploads: 4 by default (`--upload-jobs`).
- Worker project cache: 100 GB, 14-day TTL, worker-configurable.
- iOS and macOS builds always run on the Mac worker; the Linux containers only host the API,
  database, storage and TLS proxy.
