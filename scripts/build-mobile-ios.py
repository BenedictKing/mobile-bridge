#!/usr/bin/env python3
"""Build the unsigned iOS app bundle and package it as an IPA."""

from __future__ import annotations

import argparse
import hashlib
import plistlib
import shutil
import subprocess
import tempfile
import zipfile
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
IOS_ROOT = ROOT / "mobile" / "ios"
PROJECT = IOS_ROOT / "BridgePreview.xcodeproj"
APP_ID = "com.mobilebridge.app.ios"
VERSION = (ROOT / "VERSION").read_text(encoding="utf-8").strip().lstrip("v")
BUILD = "1"


def run_build(work: Path) -> Path:
    products = work / "products"
    intermediates = work / "intermediates"
    module_cache = work / "module-cache"
    subprocess.run(
        [
            "xcodebuild",
            "-project",
            str(PROJECT),
            "-target",
            "BridgePreview",
            "-configuration",
            "Release",
            "-sdk",
            "iphoneos",
            f"SYMROOT={products}",
            f"OBJROOT={intermediates}",
            f"CLANG_MODULE_CACHE_PATH={module_cache}",
            "CODE_SIGNING_ALLOWED=NO",
            "CODE_SIGNING_REQUIRED=NO",
            "CODE_SIGN_IDENTITY=",
            "build",
        ],
        cwd=ROOT,
        check=True,
    )
    return products / "Release-iphoneos" / "BridgePreview.app"


def verify_app(app: Path) -> None:
    bundles = [
        (app, APP_ID),
        (app / "PlugIns" / "BridgeActivity.appex", APP_ID + ".activity"),
        (app / "PlugIns" / "BridgeNotifications.appex", APP_ID + ".notifications"),
    ]
    for bundle, identifier in bundles:
        info_path = bundle / "Info.plist"
        info = plistlib.loads(info_path.read_bytes())
        if info.get("CFBundleIdentifier") != identifier:
            raise RuntimeError(f"Bundle identifier mismatch: {bundle}")
        if info.get("CFBundleShortVersionString") != VERSION or info.get("CFBundleVersion") != BUILD:
            raise RuntimeError(f"Bundle version mismatch: {bundle}")
        executable = bundle / info["CFBundleExecutable"]
        if not executable.is_file() or executable.stat().st_size == 0:
            raise RuntimeError(f"Missing iOS executable: {executable}")
        if (bundle / "embedded.mobileprovision").exists() or (bundle / "_CodeSignature").exists():
            raise RuntimeError(f"Unsigned build contains signing data: {bundle}")
        subprocess.run(["xcrun", "lipo", str(executable), "-verify_arch", "arm64"], check=True)
    for resource in ("channel-link.js", "mobile-ui.js", "mobile-clipboard.js", "mobile-system-bars.js", "codex.png", "claude.png", "deepseek.png", "zcode.png"):
        if not (app / resource).is_file():
            raise RuntimeError(f"Missing iOS resource: {resource}")


def package(app: Path, output: Path) -> Path:
    payload = output.parent / "Payload"
    if payload.exists():
        shutil.rmtree(payload)
    payload.mkdir(parents=True)
    shutil.copytree(app, payload / app.name)
    with tempfile.NamedTemporaryFile(suffix=".ipa", dir=output.parent, delete=False) as temporary:
        ipa = Path(temporary.name)
    with zipfile.ZipFile(ipa, "w", compression=zipfile.ZIP_DEFLATED) as archive:
        for path in sorted(payload.rglob("*")):
            if path.is_file():
                archive.write(path, path.relative_to(output.parent))
    with zipfile.ZipFile(ipa) as archive:
        invalid = archive.testzip()
    if invalid is None:
        output.parent.mkdir(parents=True, exist_ok=True)
        ipa.replace(output)
    else:
        raise RuntimeError("Invalid IPA archive")
    shutil.rmtree(payload)
    return output


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", type=Path, default=ROOT / "dist")
    args = parser.parse_args()
    if shutil.which("xcodebuild") is None:
        raise SystemExit("xcodebuild is required; install Xcode and the iOS platform first")
    args.output_dir.mkdir(parents=True, exist_ok=True)
    (ROOT / ".tmp").mkdir(parents=True, exist_ok=True)
    output = args.output_dir / f"mobile-bridge-{VERSION}-iOS-unsigned.ipa"
    with tempfile.TemporaryDirectory(prefix="mobile-bridge-ios-", dir=ROOT / ".tmp") as temporary:
        app = run_build(Path(temporary))
        verify_app(app)
        package(app, output)
    print(output)
    print("SHA-256", hashlib.sha256(output.read_bytes()).hexdigest())


if __name__ == "__main__":
    main()
