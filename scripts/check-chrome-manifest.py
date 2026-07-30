#!/usr/bin/env python3
"""Validate a staged Chrome manifest.

This runs against dist/chrome/manifest.json rather than the source file, so it
checks what would actually be loaded. Every rule here is one that Chrome either
accepts silently or fails obscurely, which is why they are asserted rather than
left to a reviewer.
"""

import json
import sys

# storage.session exists from Chrome 102, but the Promise form of
# idle.queryState() lands in 116 and is the latest gating API this runtime
# awaits. Pinned here so the number cannot quietly drift from its reason.
MINIMUM_CHROME_VERSION = "116"

# Every loopback form the daemon may bind. Declaring fewer leaves the extension
# unable to request access to a config the daemon considers perfectly valid, and
# a blocked request looks exactly like a dead daemon.
LOOPBACK_ORIGINS = {"http://127.0.0.1/*", "http://localhost/*", "http://[::1]/*"}

# Keys Firefox needs and Chrome must never see.
GECKO_ONLY_KEYS = ("browser_specific_settings", "data_collection_permissions")

REQUIRED_PERMISSIONS = ["idle", "storage", "tabs"]


def problems_with(manifest: dict) -> list[str]:
    found = []
    background = manifest.get("background", {})

    if manifest.get("manifest_version") != 3:
        found.append("manifest_version must be 3")
    if background.get("service_worker") != "background-cr.js":
        found.append("background must be the background-cr.js service worker")
    if "scripts" in background:
        found.append("background.scripts is a Firefox event-page key")
    if manifest.get("minimum_chrome_version") != MINIMUM_CHROME_VERSION:
        found.append(
            f"minimum_chrome_version must be {MINIMUM_CHROME_VERSION}, "
            "the Promise idle.queryState floor"
        )
    found.extend(
        f"{key} is Gecko-only and must not ship to Chrome"
        for key in GECKO_ONLY_KEYS
        if key in manifest
    )
    if sorted(manifest.get("permissions", [])) != REQUIRED_PERMISSIONS:
        found.append("permissions must be exactly tabs, storage, idle")
    if set(manifest.get("optional_host_permissions", [])) != LOOPBACK_ORIGINS:
        found.append("optional_host_permissions must cover every loopback form")
    return found


def main(argv: list[str]) -> int:
    if len(argv) != 2:
        print(f"usage: {argv[0]} <staged manifest.json>", file=sys.stderr)
        return 2

    try:
        with open(argv[1], encoding="utf-8") as handle:
            manifest = json.load(handle)
    except (OSError, json.JSONDecodeError) as err:
        print(f"error: cannot read {argv[1]}: {err}", file=sys.stderr)
        return 1

    found = problems_with(manifest)
    for problem in found:
        print(f"chrome manifest invalid: {problem}", file=sys.stderr)
    return 1 if found else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
