# Service, caching and performance

## Health and discovery

- `GET /healthz` → `{"status":"ok"}`.
- `GET /` → machine-readable endpoint discovery JSON.
- `GET /llms.txt` → the documentation index.
- `GET /docs/{name}.md` → one of these documents.
- `GET /client/signcloud_ai.py` → the cross-platform AI client.

Build management is API-only; there is no web administration panel. The `/install/{token}` page
only installs one completed iPhone build. A Mac worker polls the service; clients never connect to
the worker directly.

## Caching

The fast path preserves the source tree, file mtimes, DerivedData, SwiftPM `SourcePackages` and the
Xcode Compilation Cache under the stable cache key. The client caches local file hashes, uploads
only missing blobs, batches larger changes, and resumes interrupted artifact downloads.

When the complete source manifest, BuildSpec, dependency locks, Xcode toolchain and signing state
match the most recent successful build for that cache key, the worker reuses the immutable artifact
and skips Xcode entirely. Unlocked SwiftPM, CocoaPods, npm, yarn, pnpm or Bun dependency inputs
disable result reuse. Expo builds separately reuse locked JavaScript dependencies and CocoaPods
installations when application source changes.

Use the same `--cache-key` for successive builds of one project; change it to force an independent
cold cache. Add `--clean` only when a genuinely clean Xcode build is required — it deletes that
key's DerivedData while keeping content-addressed source transfer. Use `--full-upload` only to
exercise the legacy ZIP path.

## Measured timings

Representative 952-file, multi-target native iOS project, July 30, 2026:

- Initial CAS upload: 49.5 MB of missing source compressed to 23.9 MB in 1.48 s.
- Standard warm Archive/Export: 199.5 s.
- Fast Development build after cache warm-up: 11.3 s.
- One Objective-C file changed: 9.3 KB uploaded in 0.06 s; complete signed IPA in 12.0 s.

These are measurements from the current worker, not guaranteed latency. Cold builds still compile
all native and SwiftPM dependencies.

## Limits

- Maximum upload: 2 GB.
- Default client wait timeout: 2700 seconds.
- Parallel blob uploads: 4 (`--upload-jobs`).
- Eight or more missing blobs go in one compressed batch request; smaller changes use parallel
  individual uploads.
- Worker project cache: 100 GB and 14-day TTL by default, worker-configurable.
- iOS and macOS builds always execute on the trusted Mac worker with Xcode; Linux/OrbStack
  containers host only the API, database, storage and TLS proxy.
