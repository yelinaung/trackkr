#!/usr/bin/env bash
# Validate the staged Chrome package.
#
# This runs against dist/chrome/ rather than extension/, so it checks what would
# actually be loaded. Checking the source directory instead would pass while the
# package shipped something else entirely.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_dir="$(cd "${script_dir}/.." && pwd)"
stage_dir="${repo_dir}/dist/chrome"

fail() {
  echo "chrome package invalid: $*" >&2
  exit 1
}

[[ -d "${stage_dir}" ]] || fail "run mise ext-build-chrome first; ${stage_dir} does not exist"

expected=(
  manifest.json
  logic.js
  common.js
  background-core.js
  background-cr.js
  popup.html
  popup.js
  popup.css
  options.html
  options.js
  options.css
  icons/icon-48.png
  icons/icon-96.png
)

# Exactly the allowlist: nothing missing, and nothing extra. "Nothing extra" is
# the half that catches a stale staging directory or a leaked test fixture.
actual="$(cd "${stage_dir}" && find . -type f | sed 's|^\./||' | sort)"
want="$(printf '%s\n' "${expected[@]}" | sort)"
if [[ "${actual}" != "${want}" ]]; then
  echo "staged contents differ from the allowlist:" >&2
  diff <(printf '%s\n' "${want}") <(printf '%s\n' "${actual}") >&2 || true
  exit 1
fi

python3 - "${stage_dir}/manifest.json" <<'PY'
import json, sys

manifest = json.load(open(sys.argv[1]))
problems = []

if manifest.get("manifest_version") != 3:
    problems.append("manifest_version must be 3")
if manifest.get("background", {}).get("service_worker") != "background-cr.js":
    problems.append("background must be the background-cr.js service worker")
if "scripts" in manifest.get("background", {}):
    problems.append("background.scripts is a Firefox event-page key")
if manifest.get("minimum_chrome_version") != "116":
    problems.append("minimum_chrome_version must be 116, the Promise idle.queryState floor")

for key in ("browser_specific_settings", "data_collection_permissions"):
    if key in manifest:
        problems.append(f"{key} is Gecko-only and must not ship to Chrome")

if sorted(manifest.get("permissions", [])) != ["idle", "storage", "tabs"]:
    problems.append("permissions must be exactly tabs, storage, idle")

# Every loopback form the daemon may bind must be requestable, or a valid
# extension_addr leaves the extension unable to ask for access.
wanted_origins = {"http://127.0.0.1/*", "http://localhost/*", "http://[::1]/*"}
if set(manifest.get("optional_host_permissions", [])) != wanted_origins:
    problems.append("optional_host_permissions must cover every loopback form")

if problems:
    for problem in problems:
        print(f"chrome manifest invalid: {problem}", file=sys.stderr)
    sys.exit(1)
PY

version="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["version"])' "${stage_dir}/manifest.json")"
archive="${repo_dir}/dist/trackkr-chrome-${version}.zip"
[[ -f "${archive}" ]] || fail "${archive} is missing"
unzip -tqq "${archive}" || fail "${archive} failed its integrity check"

echo "chrome package valid: ${#expected[@]} files, archive ${archive}"
