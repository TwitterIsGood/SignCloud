# SignCloud Platform Optimization Requirements

## Objective

SignCloud is a general-purpose cloud build, signing, packaging, and distribution
platform. Performance work must improve reusable platform capabilities rather
than optimize for one application, repository, customer, bundle identifier, or
source layout.

## Mandatory Constraints

- Runtime behavior must never branch on an application name, repository path,
  bundle identifier, source filename, or customer identity.
- Optimization decisions may use only declared build parameters, discovered
  project capabilities, content manifests, caller capabilities, signing mode,
  dependency metadata, and the active toolchain.
- The platform must not rewrite caller source code or Xcode project files as an
  implicit optimization.
- Project-specific issues may be reported as diagnostics, but must not become
  hidden worker assumptions or hardcoded execution paths.
- Every fast path must have explicit eligibility checks and a correct standard
  fallback.
- Cache entries must be isolated by complete build identity and must never leak
  build products, signing state, or generated files between projects.
- Performance changes must preserve signing correctness, entitlements, embedded
  extensions, provisioning profiles, and artifact installability.

## Platform Optimization Layers

1. Source transport
   - Content-addressed manifests and missing-blob upload.
   - Compressed batch requests and resumable transfer.
   - Content-defined chunking for large files when file-level CAS is insufficient.

2. Build identity and result reuse
   - Fingerprint source manifest, BuildSpec, dependency locks, Xcode/SDK version,
     signing mode, and relevant environment inputs.
   - Reuse an immutable successful artifact only when the complete fingerprint
     matches.

3. Xcode execution
   - Stable per-project DerivedData and toolchain-partitioned compilation cache.
   - Dependency lock reuse and cache-key affinity.
   - Capability-driven selection between fast Development packaging and standard
     Archive/Export.
   - No application-specific build-setting overrides.

4. Artifact transport and installation
   - HTTP range requests, resume support, immutable artifact caching, and HTTP/3.
   - Generic `.app` manifest and block-delta transport for repeated Simulator or
     connected-device deployment.
   - Full IPA and OTA remain the compatibility path.

5. Worker scheduling
   - Route repeated cache keys to workers that already hold the relevant cache.
   - Separate single-build latency optimization from multi-user concurrency.
   - Enforce per-project locking and bounded cache eviction.

6. Observability
   - Record source scan, upload, queue, source sync, dependency resolution,
     compile, link, signing, packaging, artifact upload, download, and install
     timings independently.
   - Report cache hits and invalidation reasons without relying on project names.

## Acceptance Matrix

Every platform optimization must be tested against multiple project classes:

- Native Swift project.
- Native Objective-C project.
- Mixed Swift and Objective-C project.
- Multi-target project containing extensions.
- SwiftPM project.
- CocoaPods workspace.
- Expo or React Native project.
- Simulator and signed physical-device outputs.
- Clean, warm, no-change, one-file-change, file-removal, and dependency-change cases.

An optimization is not considered platform-ready when it only succeeds on the
benchmark project used during development.

## Performance Targets

These are platform targets, not guarantees for every project:

- Identical build fingerprint: return a reusable artifact in under 1 second.
- Warm one-file native change: signed Development artifact in 6-15 seconds.
- Warm Simulator build and local launch: 10-20 seconds where endpoint bandwidth
  and Simulator startup permit.
- Warm connected-device or OTA development cycle: 15-30 seconds where device
  installation and network conditions permit.
- Cold builds are measured and optimized separately because dependency and
  native compilation cost cannot be eliminated by transport caching.

## Prioritized Roadmap

1. Complete build fingerprint and immutable result reuse.
2. Capability engine with fast-path eligibility and automatic standard fallback.
3. Generic artifact manifest and content-defined block delta protocol.
4. Endpoint capability reporting for OS, architecture, Xcode, Simulator, and device.
5. Cache-affinity worker scheduling and queue metrics.
6. Cross-project benchmark suite covering the acceptance matrix.

## Implemented Baseline

- File-level CAS source manifests with compressed batch upload.
- Deduplicated, bounded-parallel worker blob prefetch for cold source sync.
- Stable per-cache-key source trees and DerivedData.
- Direct unique shared-Scheme discovery with Xcode fallback for ambiguous workspaces.
- Toolchain-partitioned Xcode Compilation Cache.
- Capability-gated fast Development packaging with Archive/Export fallback.
- Complete-input recent-result reuse with dependency-lock and signing-state
  validation.
- Locked Expo JavaScript and CocoaPods dependency caches.
- Dependency-download proxy support and dedicated npm/CocoaPods cache roots.
- Build-strategy, cache, stage timing, CPU, and peak-memory metrics for successful
  and failed builds.
