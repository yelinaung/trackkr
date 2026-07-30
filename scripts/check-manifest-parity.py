#!/usr/bin/env python3
"""Fail when the Chrome manifest has drifted from the Firefox one.

The two are separate source files by necessity: web-ext validates only the
Firefox manifest, and Chrome rejects the Gecko keys it carries. Nothing else
stops a release that bumps one from shipping the other stale, and the package is
the artifact nobody reads before loading it -- so everything they must agree on
is compared rather than trusted.
"""

import json
import sys

# Fields that describe the same extension either way. Anything absent from both
# still counts as agreeing, which is what lets a key be dropped from both at
# once without a special case here.
SHARED_FIELDS = (
    "name",
    "version",
    "description",
    "permissions",
    "optional_host_permissions",
    "icons",
    # The popup and options pages are the same files in both packages.
    "action",
    "options_ui",
)


def load(path: str) -> dict:
    with open(path, encoding="utf-8") as handle:
        return json.load(handle)


def drift(firefox: dict, chrome: dict) -> list[str]:
    return [
        f"{field}: firefox {firefox.get(field)!r} != chrome {chrome.get(field)!r}"
        for field in SHARED_FIELDS
        if firefox.get(field) != chrome.get(field)
    ]


def main(argv: list[str]) -> int:
    if len(argv) != 3:
        print(f"usage: {argv[0]} <manifest.json> <manifest.chrome.json>", file=sys.stderr)
        return 2

    try:
        firefox, chrome = load(argv[1]), load(argv[2])
    except (OSError, json.JSONDecodeError) as err:
        print(f"error: cannot read a manifest: {err}", file=sys.stderr)
        return 1

    problems = drift(firefox, chrome)
    if problems:
        print("error: the Chrome manifest has drifted from manifest.json", file=sys.stderr)
        for problem in problems:
            print(f"  {problem}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
