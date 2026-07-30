#!/usr/bin/env python3
"""Print one top-level field from an extension manifest.

Both the Chrome build and its validator need the version to name the archive,
and reading JSON with shell tools means either adding a jq dependency or
parsing JSON with a regex.
"""

import json
import sys


def main(argv: list[str]) -> int:
    if len(argv) != 3:
        print(f"usage: {argv[0]} <manifest.json> <field>", file=sys.stderr)
        return 2

    path, field = argv[1], argv[2]
    try:
        with open(path, encoding="utf-8") as handle:
            manifest = json.load(handle)
    except (OSError, json.JSONDecodeError) as err:
        print(f"error: cannot read {path}: {err}", file=sys.stderr)
        return 1

    if field not in manifest:
        print(f"error: {path} has no {field}", file=sys.stderr)
        return 1

    print(manifest[field])
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
