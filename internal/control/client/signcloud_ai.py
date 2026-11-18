#!/usr/bin/env python3
"""Submit an iOS or macOS build to SignCloud and wait for the result.

This wrapper intentionally reads credentials only from environment variables so
an AI coding agent never needs to place the API token in source code or prompts.
"""

from __future__ import annotations

import argparse
from concurrent.futures import ThreadPoolExecutor, as_completed
import hashlib
import json
import os
from pathlib import Path
import platform
import shutil
import stat
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
import zipfile


EXCLUDED_DIRS = {
    ".git",
    ".gradle",
    ".idea",
    ".expo",
    ".next",
    ".turbo",
    "DerivedData",
    "build",
    "dist",
    "node_modules",
    ".signcloud",
}


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Submit a project or IPA to SignCloud")
    parser.add_argument("path", nargs="?", type=Path, help="project directory, source zip, or IPA")
    parser.add_argument("--kind", choices=("auto", "xcode", "expo", "resign", "simulator", "macos", "resolve", "verify"), default="auto")
    parser.add_argument("--name", help="build display name")
    parser.add_argument("--scheme", help="shared Xcode scheme")
    parser.add_argument("--workspace", help="relative .xcworkspace path")
    parser.add_argument("--project", help="relative .xcodeproj path")
    parser.add_argument("--configuration", help="Xcode configuration, such as Debug or Release")
    parser.add_argument("--team-id", help="Apple Developer Team ID override")
    parser.add_argument("--export-method", default="development", choices=("development", "ad-hoc", "app-store", "enterprise"))
    parser.add_argument("--no-provisioning-updates", action="store_true", help="do not let Xcode update signing resources")
    parser.add_argument("--no-wait", action="store_true", help="return immediately after submission")
    parser.add_argument("--install-simulator", action="store_true", help="install and launch a simulator artifact on this Mac")
    parser.add_argument("--cache-key", help="stable project cache key; defaults to the project directory name")
    parser.add_argument("--full-upload", action="store_true", help="use legacy full ZIP upload")
    parser.add_argument("--upload-jobs", type=int, default=4, help="parallel blob uploads (default: 4)")
    parser.add_argument("--clean", action="store_true", help="discard this project's DerivedData before building")
    parser.add_argument("--standard-export", action="store_true", help="use Xcode archive/export instead of fast development packaging")
    parser.add_argument("--standard-build", action="store_true", help="keep every Xcode build setting; skip the fast Debug build profile")
    parser.add_argument("--architectures", help="macOS architectures, comma-separated: arm64,x86_64")
    parser.add_argument("--signing", choices=("adhoc", "developer-id"), help="macOS signing mode")
    parser.add_argument("--notarize", action="store_true", help="submit a Developer ID macOS artifact to Apple notarization")
    parser.add_argument("--artifact-format", choices=("dmg", "app-zip"), help="macOS primary artifact format (default: dmg)")
    parser.add_argument("--include-app-zip", action="store_true", help="also return a zipped macOS .app")
    parser.add_argument("--no-hardened-runtime", action="store_true", help="disable Hardened Runtime for a non-notarized macOS build")
    parser.add_argument("--verify-script", help="verify: .ad or Maestro .yaml flow inside the project")
    parser.add_argument("--device", help="verify: simulator model (default: iPhone 16)")
    parser.add_argument("--device-os", help="verify: OS version or range, such as 26.5, >=26 or 18 - 26")
    parser.add_argument("--device-mode", choices=("slim", "full"), default="slim", help="verify: slim (default) or full simulator")
    parser.add_argument("--record-video", action="store_true", help="verify: record a screen video as evidence")
    parser.add_argument("--verify-timeout", type=int, default=900, help="verify timeout in seconds (default: 900)")
    parser.add_argument("--output", type=Path, help="macOS artifact download path")
    parser.add_argument("--timeout", type=int, default=2700, help="wait timeout in seconds (default: 2700)")
    parser.add_argument("--list-local-devices", action="store_true", help="list paired physical iPhones on this Mac")
    parser.add_argument("--list-registered-devices", action="store_true", help="list devices enrolled through this API token")
    parser.add_argument("--enroll-device", action="store_true", help="enroll a paired physical iPhone in the Apple Developer team")
    parser.add_argument("--device-udid", help="specific local iPhone UDID to enroll when more than one is paired")
    parser.add_argument("--device-index", type=int, help="1-based index from --list-local-devices; avoids exposing the full UDID")
    parser.add_argument("--device-name", help="name shown in Apple Developer and SignCloud")
    parser.add_argument("--yes", action="store_true", help="confirm uploading the selected device UDID without prompting")
    return parser.parse_args()


def detect_kind(path: Path) -> str:
    if path.suffix.lower() == ".ipa":
        return "resign"
    if path.is_dir() and (path / "package.json").exists():
        try:
            package = json.loads((path / "package.json").read_text(encoding="utf-8"))
            dependencies = {**package.get("dependencies", {}), **package.get("devDependencies", {})}
            if "expo" in dependencies:
                return "expo"
        except (OSError, json.JSONDecodeError):
            pass
    return "xcode"


def write_zip_symlink(archive: zipfile.ZipFile, path: Path, relative: Path) -> None:
    target = os.readlink(path)
    if target.startswith("/") or ".." in Path(target).parts:
        return
    info = zipfile.ZipInfo(relative.as_posix())
    info.create_system = 3
    info.external_attr = (stat.S_IFLNK | 0o777) << 16
    archive.writestr(info, target)


def package_project(source: Path, destination: Path) -> None:
    with zipfile.ZipFile(destination, "w", zipfile.ZIP_DEFLATED, allowZip64=True) as archive:
        for root, dirs, files in os.walk(source):
            dirs[:] = [name for name in dirs if name not in EXCLUDED_DIRS]
            root_path = Path(root)
            for name in list(dirs):
                path = root_path / name
                if path.is_symlink():
                    write_zip_symlink(archive, path, path.relative_to(source.parent))
                    dirs.remove(name)
            for filename in files:
                path = root_path / filename
                relative = path.relative_to(source.parent)
                if path.is_symlink():
                    write_zip_symlink(archive, path, relative)
                    continue
                archive.write(path, relative.as_posix())


def hash_cache_path(source: Path) -> Path:
    identity = hashlib.sha256(str(source).encode("utf-8")).hexdigest()[:24]
    return Path.home() / ".cache" / "signcloud" / "hashes" / f"{identity}.json"


def load_hash_cache(source: Path) -> dict:
    path = hash_cache_path(source)
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return {}


def save_hash_cache(source: Path, cache: dict) -> None:
    path = hash_cache_path(source)
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(".tmp")
    temporary.write_text(json.dumps(cache, separators=(",", ":")), encoding="utf-8")
    temporary.replace(path)


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        while chunk := source.read(1024 * 1024):
            digest.update(chunk)
    return digest.hexdigest()


def build_manifest(source: Path, extra_excluded_dirs: set[str] | None = None) -> tuple[list[dict], dict[str, Path], int]:
    excluded_dirs = EXCLUDED_DIRS | (extra_excluded_dirs or set())
    previous_cache = load_hash_cache(source)
    next_cache: dict[str, dict] = {}
    files: list[dict] = []
    blobs: dict[str, Path] = {}
    hashed_bytes = 0
    for root, dirs, names in os.walk(source):
        dirs[:] = sorted(name for name in dirs if name not in excluded_dirs)
        root_path = Path(root)
        for name in list(dirs):
            path = root_path / name
            if path.is_symlink():
                relative = path.relative_to(source).as_posix()
                files.append({"path": relative, "hash": "", "size": 0, "mode": 0, "link": os.readlink(path)})
                dirs.remove(name)
        for name in sorted(names):
            path = root_path / name
            if path.is_symlink():
                relative = path.relative_to(source).as_posix()
                files.append({"path": relative, "hash": "", "size": 0, "mode": 0, "link": os.readlink(path)})
                continue
            if not path.is_file():
                continue
            relative = path.relative_to(source).as_posix()
            stat = path.stat()
            signature = f"{stat.st_size}:{stat.st_mtime_ns}"
            cached = previous_cache.get(relative, {})
            if cached.get("signature") == signature and cached.get("hash"):
                digest = cached["hash"]
            else:
                digest = sha256_file(path)
                hashed_bytes += stat.st_size
            next_cache[relative] = {"signature": signature, "hash": digest}
            files.append({"path": relative, "hash": digest, "size": stat.st_size, "mode": stat.st_mode & 0o777})
            blobs.setdefault(digest, path)
    if not files:
        raise RuntimeError("project directory contains no uploadable files")
    save_hash_cache(source, next_cache)
    return files, blobs, hashed_bytes


def post_json(server: str, token: str, path: str, payload: dict) -> dict:
    data = json.dumps(payload, separators=(",", ":")).encode("utf-8")
    request = urllib.request.Request(
        server + path,
        data=data,
        method="POST",
        headers={
            "Authorization": "Bearer " + token,
            "Content-Type": "application/json",
            "Accept": "application/json",
        },
    )
    try:
        with urllib.request.urlopen(request, timeout=120) as response:
            return json.load(response)
    except urllib.error.HTTPError as error:
        body = error.read().decode("utf-8", errors="replace")
        raise RuntimeError(f"SignCloud returned HTTP {error.code}: {body}") from error


def upload_blob(server: str, token: str, digest: str, path: Path) -> None:
    curl = shutil.which("curl")
    if not curl:
        raise RuntimeError("curl is required")
    result = subprocess.run(
        [
            curl,
            "--fail-with-body",
            "--silent",
            "--show-error",
            "-X",
            "PUT",
            "-H",
            "Authorization: Bearer " + token,
            "-H",
            "Content-Type: application/octet-stream",
            "--data-binary",
            "@" + str(path),
            server + "/v1/blobs/" + digest,
        ],
        check=False,
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        raise RuntimeError(f"upload blob {digest[:12]} failed: {result.stderr.strip() or result.stdout.strip()}")


def upload_blob_batch(server: str, token: str, missing: list[str], blobs: dict[str, Path]) -> tuple[int, int]:
    curl = shutil.which("curl")
    if not curl:
        raise RuntimeError("curl is required")
    with tempfile.TemporaryDirectory(prefix="signcloud-blobs-") as temporary:
        archive_path = Path(temporary) / "blobs.zip"
        with zipfile.ZipFile(archive_path, "w", zipfile.ZIP_DEFLATED, allowZip64=True, compresslevel=6) as archive:
            for digest in missing:
                archive.write(blobs[digest], digest)
        compressed_size = archive_path.stat().st_size
        result = subprocess.run(
            [
                curl,
                "--fail-with-body",
                "--silent",
                "--show-error",
                "-X",
                "PUT",
                "-H",
                "Authorization: Bearer " + token,
                "-H",
                "Content-Type: application/zip",
                "--data-binary",
                "@" + str(archive_path),
                server + "/v1/blobs/batch",
            ],
            check=False,
            capture_output=True,
            text=True,
        )
        if result.returncode != 0:
            raise RuntimeError(result.stderr.strip() or result.stdout.strip() or "batch blob upload failed")
        response = json.loads(result.stdout)
        return compressed_size, int(response.get("uploaded", 0))


def submit_incremental(
    server: str,
    token: str,
    source: Path,
    name: str,
    kind: str,
    spec: dict,
    cache_key: str,
    upload_jobs: int,
) -> dict:
    scan_started = time.monotonic()
    files, blobs, hashed_bytes = build_manifest(source, {"android"} if kind == "expo" else None)
    scan_seconds = time.monotonic() - scan_started
    manifest = {"version": 1, "cache_key": cache_key, "files": files}
    plan = post_json(server, token, "/v1/uploads/plan", manifest)
    missing = plan.get("missing", [])
    print(
        f"Manifest: {len(files)} files, hashed {hashed_bytes} bytes in {scan_seconds:.2f}s; "
        f"upload {plan.get('missing_files', len(missing))} files / {plan.get('missing_bytes', 0)} bytes, "
        f"reuse {plan.get('reused_bytes', 0)} bytes",
        flush=True,
    )
    if len(missing) >= 8:
        upload_started = time.monotonic()
        compressed_size, uploaded = upload_blob_batch(server, token, missing, blobs)
        print(
            f"Batch uploaded {uploaded}/{len(missing)} blobs, compressed to {compressed_size} bytes "
            f"in {time.monotonic() - upload_started:.2f}s",
            flush=True,
        )
    elif missing:
        upload_started = time.monotonic()
        with ThreadPoolExecutor(max_workers=max(1, upload_jobs)) as executor:
            futures = {executor.submit(upload_blob, server, token, digest, blobs[digest]): digest for digest in missing}
            completed = 0
            for future in as_completed(futures):
                future.result()
                completed += 1
                if completed == len(missing) or completed % 25 == 0:
                    print(f"Uploaded {completed}/{len(missing)} blobs", flush=True)
        print(f"Blob upload completed in {time.monotonic() - upload_started:.2f}s", flush=True)
    return post_json(
        server,
        token,
        "/v1/builds/incremental",
        {"name": name, "kind": kind, "cache_key": cache_key, "spec": spec, "files": files},
    )


def request_json(server: str, token: str, path: str):
    request = urllib.request.Request(
        server + path,
        headers={"Authorization": "Bearer " + token, "Accept": "application/json"},
    )
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            return json.load(response)
    except urllib.error.HTTPError as error:
        body = error.read().decode("utf-8", errors="replace")
        raise RuntimeError(f"SignCloud returned HTTP {error.code}: {body}") from error


def local_physical_iphones() -> list[dict]:
    if platform.system() != "Darwin" or not shutil.which("xcrun"):
        raise RuntimeError("local iPhone detection requires macOS with Xcode Command Line Tools")
    with tempfile.TemporaryDirectory(prefix="signcloud-devices-") as temporary:
        output = Path(temporary) / "devices.json"
        result = subprocess.run(
            [
                "xcrun",
                "devicectl",
                "list",
                "devices",
                "--json-output",
                str(output),
                "--timeout",
                "15",
                "--quiet",
            ],
            check=False,
            capture_output=True,
            text=True,
        )
        if result.returncode != 0:
            raise RuntimeError(result.stderr.strip() or "xcrun devicectl could not list devices")
        try:
            payload = json.loads(output.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as error:
            raise RuntimeError("could not read devicectl JSON output") from error
    devices = []
    for item in payload.get("result", {}).get("devices", []):
        hardware = item.get("hardwareProperties", {})
        properties = item.get("deviceProperties", {})
        connection = item.get("connectionProperties", {})
        if str(hardware.get("platform", "")).lower() != "ios":
            continue
        if str(hardware.get("reality", "")).lower() != "physical":
            continue
        udid = str(hardware.get("udid", "")).strip().upper()
        if not udid:
            continue
        devices.append(
            {
                "udid": udid,
                "name": str(properties.get("name") or hardware.get("marketingName") or "iPhone"),
                "model": str(hardware.get("marketingName") or hardware.get("productType") or "iPhone"),
                "os_version": str(properties.get("osVersionNumber") or ""),
                "paired": connection.get("pairingState") == "paired",
            }
        )
    return devices


def masked_udid(udid: str) -> str:
    return udid[:8] + "..." + udid[-6:] if len(udid) > 14 else "***"


def print_local_devices(devices: list[dict]) -> None:
    if not devices:
        print("No paired physical iPhones found")
        return
    for index, device in enumerate(devices, 1):
        print(
            f"{index}. {device['name']} | {device['model']} | iOS {device['os_version'] or '?'} | "
            f"UDID {masked_udid(device['udid'])} | paired={str(device['paired']).lower()}"
        )


def select_local_device(devices: list[dict], requested_udid: str | None, requested_index: int | None) -> dict:
    if requested_udid and requested_index:
        raise RuntimeError("use either --device-index or --device-udid, not both")
    if requested_index is not None:
        if requested_index < 1 or requested_index > len(devices):
            raise RuntimeError("--device-index is outside the local iPhone list")
        return devices[requested_index - 1]
    if requested_udid:
        normalized = requested_udid.strip().upper()
        for device in devices:
            if device["udid"] == normalized:
                return device
        raise RuntimeError("the requested UDID is not present in the paired local iPhone list")
    if len(devices) == 1:
        return devices[0]
    if not devices:
        raise RuntimeError("no paired physical iPhone was found; connect and trust the iPhone first")
    raise RuntimeError("multiple iPhones are paired; rerun with --device-index after listing local devices")


def enroll_device(server: str, token: str, args: argparse.Namespace) -> int:
    device = select_local_device(local_physical_iphones(), args.device_udid, args.device_index)
    print(
        f"Selected: {device['name']} ({device['model']}), UDID {masked_udid(device['udid'])}",
        flush=True,
    )
    if not args.yes:
        if not sys.stdin.isatty():
            raise RuntimeError("device enrollment uploads the iPhone UDID; rerun with --yes after user confirmation")
        answer = input("Upload this UDID to SignCloud and register it with Apple Developer? [y/N] ").strip().lower()
        if answer not in ("y", "yes"):
            print("Cancelled")
            return 2
    enrolled = post_json(
        server,
        token,
        "/v1/devices",
        {"udid": device["udid"], "name": args.device_name or device["name"], "platform": "IOS"},
    )
    print(f"Enrollment task: {enrolled['id']} ({enrolled['status']})")
    deadline = time.monotonic() + args.timeout
    previous = ""
    while time.monotonic() < deadline:
        current = request_json(server, token, "/v1/devices/" + enrolled["id"])
        if current["status"] != previous:
            print(f"Device status: {current['status']}", flush=True)
            previous = current["status"]
        if current["status"] == "registered":
            print(f"Device registered: {current['name']} ({current['masked_udid']})")
            print("Use --export-method ad-hoc --standard-export for an installable distribution build.")
            return 0
        if current["status"] == "failed":
            raise RuntimeError(current.get("error") or "device registration failed")
        time.sleep(2)
    raise RuntimeError("timed out waiting for Apple device registration")


def submit(server: str, token: str, source: Path, name: str, kind: str, spec: dict) -> dict:
    curl = shutil.which("curl")
    if not curl:
        raise RuntimeError("curl is required; it is included with current Windows and macOS versions")
    command = [
        curl,
        "--fail-with-body",
        "--silent",
        "--show-error",
        "-H",
        "Authorization: Bearer " + token,
        "-F",
        "name=" + name,
        "-F",
        "kind=" + kind,
        "-F",
        "spec=" + json.dumps(spec, separators=(",", ":")),
        "-F",
        "source=@" + str(source),
        server + "/v1/builds",
    ]
    result = subprocess.run(command, check=False, capture_output=True, text=True)
    if result.returncode != 0:
        raise RuntimeError(result.stderr.strip() or result.stdout.strip() or "curl upload failed")
    return json.loads(result.stdout)


def download(url: str, destination: Path) -> None:
    curl = shutil.which("curl")
    if curl:
        result = subprocess.run(
            [
                curl,
                "--fail-with-body",
                "--location",
                "--retry",
                "3",
                "--retry-all-errors",
                "--continue-at",
                "-",
                "--output",
                str(destination),
                url,
            ],
            check=False,
        )
        if result.returncode != 0:
            raise RuntimeError("artifact download failed")
        return
    request = urllib.request.Request(url, headers={"User-Agent": "SignCloud-AI/1.0"})
    with urllib.request.urlopen(request, timeout=300) as response, destination.open("wb") as output:
        shutil.copyfileobj(response, output)


def download_and_verify(url: str, destination: Path, expected_sha256: str) -> str:
    if not expected_sha256 or len(expected_sha256) != 64:
        raise RuntimeError("successful macOS build did not return a valid SHA-256")
    destination = destination.expanduser().resolve()
    destination.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="signcloud-download-", dir=str(destination.parent)) as temporary:
        partial = Path(temporary) / destination.name
        download(url, partial)
        actual = sha256_file(partial)
        if actual.lower() != expected_sha256.lower():
            raise RuntimeError(f"artifact SHA-256 mismatch: expected {expected_sha256}, got {actual}")
        partial.replace(destination)
    return actual


def booted_simulator() -> bool:
    result = subprocess.run(
        ["xcrun", "simctl", "list", "devices", "booted", "--json"],
        check=False,
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        return False
    data = json.loads(result.stdout)
    return any(devices for devices in data.get("devices", {}).values())


def local_simulator_arch() -> str:
    """The architecture this Mac's Simulator runs natively, or "" elsewhere."""
    if sys.platform != "darwin":
        return ""
    machine = platform.machine()
    return machine if machine in ("arm64", "x86_64") else ""


def install_simulator_artifact(artifact_url: str, bundle_id: str) -> None:
    if platform.system() != "Darwin" or not shutil.which("xcrun"):
        raise RuntimeError("simulator installation requires a Mac with Xcode")
    if not booted_simulator():
        subprocess.run(["open", "-a", "Simulator"], check=True)
        print("Waiting for iOS Simulator to boot ...", flush=True)
        for _ in range(60):
            if booted_simulator():
                break
            time.sleep(2)
        else:
            raise RuntimeError("no booted iOS Simulator found")

    with tempfile.TemporaryDirectory(prefix="signcloud-simulator-") as temporary:
        root = Path(temporary)
        archive_path = root / "artifact.zip"
        download(artifact_url, archive_path)
        with zipfile.ZipFile(archive_path) as archive:
            archive.extractall(root / "unpacked")
        apps = sorted((root / "unpacked").glob("*.app"))
        if len(apps) != 1:
            raise RuntimeError("simulator artifact must contain exactly one .app")
        subprocess.run(["xcrun", "simctl", "install", "booted", str(apps[0])], check=True)
        subprocess.run(["xcrun", "simctl", "launch", "booted", bundle_id], check=True)
        print(f"Installed and launched {bundle_id} on the booted Simulator")


def main() -> int:
    args = parse_args()
    server = os.environ.get("SIGNCLOUD_SERVER", "http://localhost:8787").rstrip("/")
    device_actions = sum((args.list_local_devices, args.list_registered_devices, args.enroll_device))
    if device_actions > 1:
        raise RuntimeError("choose only one device action")
    if args.list_local_devices:
        print_local_devices(local_physical_iphones())
        return 0

    token = os.environ.get("SIGNCLOUD_API_TOKEN", "")
    if not token:
        raise RuntimeError("SIGNCLOUD_API_TOKEN is not set")
    if args.list_registered_devices:
        devices = request_json(server, token, "/v1/devices")
        if not devices:
            print("No devices registered for this API token")
        for device in devices:
            print(f"{device['id']} | {device['name']} | {device['masked_udid']} | {device['status']}")
        return 0
    if args.enroll_device:
        return enroll_device(server, token, args)
    if args.path is None:
        raise RuntimeError("a project path is required unless a device action is selected")

    source = args.path.expanduser().resolve()
    if not source.exists():
        raise RuntimeError(f"source does not exist: {source}")

    kind = detect_kind(source) if args.kind == "auto" else args.kind
    if args.install_simulator and kind != "simulator":
        raise RuntimeError("--install-simulator requires --kind simulator")
    name = args.name or source.stem
    macos_only_options = (
        args.architectures,
        args.signing,
        args.notarize,
        args.artifact_format,
        args.include_app_zip,
        args.no_hardened_runtime,
        args.output,
    )
    if kind != "macos" and any(macos_only_options):
        raise RuntimeError("macOS signing, architecture, artifact, and output options require --kind macos")
    if kind == "macos":
        if not args.scheme:
            raise RuntimeError("--scheme is required for --kind macos")
        if bool(args.project) == bool(args.workspace):
            raise RuntimeError("--kind macos requires exactly one of --project or --workspace")
        architectures = [value.strip() for value in (args.architectures or "arm64,x86_64").split(",") if value.strip()]
        if not architectures or any(value not in ("arm64", "x86_64") for value in architectures):
            raise RuntimeError("--architectures supports only arm64,x86_64")
        signing = args.signing or "adhoc"
        if args.notarize and signing != "developer-id":
            raise RuntimeError("--notarize requires --signing developer-id")
        if args.notarize and args.no_hardened_runtime:
            raise RuntimeError("--notarize requires Hardened Runtime")
        spec = {
            "scheme": args.scheme,
            "workspace": args.workspace or "",
            "project": args.project or "",
            "configuration": args.configuration or "Release",
            "team_id": args.team_id or "",
            "architectures": architectures,
            "signing": signing,
            "notarize": args.notarize,
            "artifact_format": args.artifact_format or "dmg",
            "include_app_zip": args.include_app_zip,
            "hardened_runtime": not args.no_hardened_runtime,
            "clean": args.clean,
        }
    elif kind == "resolve":
        if args.project and args.workspace:
            raise RuntimeError("--kind resolve accepts at most one of --project or --workspace")
        spec = {
            "workspace": args.workspace or "",
            "project": args.project or "",
        }
    elif kind == "verify":
        if args.project and args.workspace:
            raise RuntimeError("--kind verify accepts at most one of --project or --workspace")
        spec = {
            "scheme": args.scheme or "",
            "workspace": args.workspace or "",
            "project": args.project or "",
            "configuration": args.configuration or "",
            "device": args.device or "",
            "device_os": args.device_os or "",
            "device_mode": args.device_mode,
            "verify_script": args.verify_script or "",
            "record_video": args.record_video,
            "verify_timeout_seconds": args.verify_timeout,
            "clean": args.clean,
        }
    else:
        spec = {
            "scheme": args.scheme or "",
            "workspace": args.workspace or "",
            "project": args.project or "",
            "configuration": args.configuration or "",
            "team_id": args.team_id or "",
            "export_method": args.export_method,
            "allow_provisioning_updates": not args.no_provisioning_updates,
            "clean": args.clean,
            "fast_package": kind in ("xcode", "expo") and args.export_method == "development" and not args.standard_export,
        }
        if kind in ("xcode", "expo", "simulator") and not args.standard_build:
            spec["build_profile"] = "fast"
            simulator_arch = local_simulator_arch() if kind == "simulator" else ""
            if simulator_arch:
                spec["simulator_arch"] = simulator_arch

    temporary: tempfile.TemporaryDirectory[str] | None = None
    upload = source
    if source.is_dir() and not args.full_upload:
        cache_key = args.cache_key or source.name
        build = submit_incremental(server, token, source, name, kind, spec, cache_key, args.upload_jobs)
    elif source.is_dir():
        temporary = tempfile.TemporaryDirectory(prefix="signcloud-")
        upload = Path(temporary.name) / (source.name + ".zip")
        print(f"Packaging {source} ...", flush=True)
        package_project(source, upload)
        try:
            build = submit(server, token, upload, name, kind, spec)
        finally:
            temporary.cleanup()
    else:
        build = submit(server, token, upload, name, kind, spec)

    build_id = build["id"]
    print(f"Submitted build {build_id} ({kind})")
    if build.get("install_url"):
        print(f"Install page: {build['install_url']}")
    if args.no_wait:
        return 0

    deadline = time.monotonic() + args.timeout
    wait_started = time.monotonic()
    previous_status = ""
    previous_stage = ""
    shown_view_url = ""
    while time.monotonic() < deadline:
        current = request_json(server, token, "/v1/builds/" + build_id)
        status = current["status"]
        view_url = current.get("view_url", "")
        if view_url and view_url != shown_view_url:
            print(f"Live view: {view_url}", flush=True)
            shown_view_url = view_url
        if status != previous_status:
            print(f"Status: {status}", flush=True)
            previous_status = status
        stage = current.get("stage", "")
        if stage and stage != previous_stage:
            print(f"Stage: {stage}", flush=True)
            previous_stage = stage
        if status == "succeeded":
            print("Verification passed" if kind == "verify" else "Build succeeded")
            if kind == "verify":
                print(f"Verdict: {current.get('verdict', '')}")
            print(f"Bundle ID: {current.get('bundle_id', '')}")
            print(f"Version: {current.get('version', '')}")
            if current.get("metrics"):
                print("Metrics: " + json.dumps(current["metrics"], ensure_ascii=False, separators=(",", ":")))
            if current.get("install_url"):
                print(f"Install page: {current['install_url']}")
            print(f"Artifact download: {current.get('artifact_url', '')}")
            if current.get("app_zip_url"):
                print(f"App ZIP download: {current['app_zip_url']}")
            if kind == "macos":
                suffix = ".dmg" if (args.artifact_format or "dmg") == "dmg" else ".app.zip"
                destination = args.output or Path.cwd() / (name + suffix)
                digest = download_and_verify(current["artifact_url"], destination, current.get("sha256", ""))
                print(f"Downloaded: {destination.expanduser().resolve()}")
                print(f"SHA-256 verified: {digest}")
            if args.install_simulator:
                install_simulator_artifact(current["artifact_url"], current["bundle_id"])
            return 0
        if status == "failed":
            print("Verification failed" if kind == "verify" else "Build failed", file=sys.stderr)
            if kind == "verify" and current.get("verdict"):
                print(f"Verdict: {current['verdict']}", file=sys.stderr)
            if current.get("error"):
                print(current["error"], file=sys.stderr)
            if current.get("artifact_url"):
                print(f"Evidence download: {current['artifact_url']}", file=sys.stderr)
            if current.get("logs"):
                print("\nLast build logs:\n" + current["logs"][-12000:], file=sys.stderr)
            return 1
        if status == "canceled":
            print("Build canceled", file=sys.stderr)
            return 1
        time.sleep(1 if time.monotonic() - wait_started < 60 else 5)

    print(
        "Timed out waiting for the build. If it remains queued, verify that a macOS Agent is running.",
        file=sys.stderr,
    )
    return 2


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (RuntimeError, OSError, json.JSONDecodeError) as error:
        print(f"signcloud-ai: {error}", file=sys.stderr)
        raise SystemExit(1)
