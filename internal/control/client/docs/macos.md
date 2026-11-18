# macOS desktop applications

Requests require exactly one relative `project` or `workspace`, an explicit shared Scheme and a
normal Xcode configuration. Architectures default to `arm64,x86_64` Universal.

Signing modes:

- `adhoc`: local/internal testing. Not Developer ID distribution and cannot be notarized.
- `developer-id`: the worker selects its configured Developer ID Application identity. The client
  cannot choose an arbitrary identity or upload a certificate/key.

The worker builds with `generic/platform=macOS`, validates requested architectures, preserves
project entitlements (App Sandbox on or off), applies Hardened Runtime by default, signs nested
code, creates the DMG with the `.app` and `/Applications` shortcut, and performs final codesign,
stapler, Gatekeeper, hdiutil, mount-content and SHA-256 checks. Only the allow-listed
`xcodegen generate` action may run when `project.yml` exists without a generated Xcode container
and the worker administrator enabled XcodeGen. There is no client prebuild shell or arbitrary
command field.

Developer ID prerequisites on the worker:

1. A valid Developer ID Application certificate and private key in the dedicated build Keychain.
2. The requested `team_id` matches that certificate.
3. The worker identity selector and non-secret signing configuration version are configured.

Notarization also needs a worker-side `notarytool` Keychain profile. Apple credentials, p8/P12
material, passwords, private keys, Keychain paths and unlock commands are never accepted in a
build spec or returned in logs/API errors. `notarize=true` requires `signing=developer-id`,
Hardened Runtime and a DMG primary artifact.

```bash
python3 "$HOME/.signcloud/signcloud_ai.py" \
  "/path/to/DeviceBridge" \
  --kind macos \
  --scheme DeviceBridge \
  --project DeviceBridge.xcodeproj \
  --configuration Release \
  --team-id YOUR_TEAM_ID \
  --cache-key devicebridge-macos \
  --name 无线调试助手 \
  --architectures arm64,x86_64 \
  --signing developer-id \
  --notarize \
  --artifact-format dmg \
  --output "./无线调试助手-1.0.dmg"
```

The client reuses content-addressed upload and the bearer token, waits for completion, downloads
the primary artifact, and rejects it if the local SHA-256 differs from the response. Add
`--include-app-zip` for a separate `app_zip_url`. For an internal build use `--signing adhoc` and
omit `--notarize`.

Stages: `preparing`, `generating_project` (only when needed), `building`, `signing`, `packaging`,
`notarizing`, `stapling`, `validating`, `uploading`.

## Common failures

- Shared Scheme not found: mark the Scheme shared and include `xcshareddata/xcschemes`.
- Developer ID identity not configured: install/configure it on the worker; never send it through
  the client.
- Team ID mismatch: submit the certificate's team or configure the correct worker identity.
- Signing failure: verify certificate validity, private-key access, nested targets, entitlements
  and Hardened Runtime.
- Apple rejected notarization: fix the sanitized issue summary, sign a new artifact, rebuild.
- Staple failure: confirm Apple reports Accepted, then retry a new task after ticket propagation.
- DMG verification failure: fix packaging, filesystem, app name or the Applications shortcut.
- Universal architecture missing: make every target and dependency support both arm64 and x86_64,
  or request one supported architecture.

## macOS cache correctness

The cache key is namespaced by the source manifest and build kind. A macOS result fingerprint also
includes the Scheme, selected project/workspace, configuration, requested architectures, full spec,
Xcode version, macOS SDK version, deployment/build settings in project files, SwiftPM/CocoaPods
lock files, entitlements, Info.plist, signing mode, Team ID, notarization flag, worker signing
configuration version and the non-secret certificate identity state. Source build scripts and
their inputs are part of the manifest. Any of those changes prevents reuse of an old signed
artifact. Use `--clean` only when DerivedData itself must be discarded.
