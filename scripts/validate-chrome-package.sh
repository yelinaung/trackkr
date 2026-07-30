#!/usr/bin/env bash
# Validate the staged Chrome package.
#
# This runs against dist/chrome/ rather than extension/, so it checks what would
# actually be loaded. Checking the source directory instead would pass while the
# package shipped something else entirely.
set -euo pipefail

command -v python3 >/dev/null || {
  echo "error: python3 is required to validate the manifest" >&2
  exit 1
}

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

"${script_dir}/check-chrome-manifest.py" "${stage_dir}/manifest.json"

version="$("${script_dir}/manifest-field.py" "${stage_dir}/manifest.json" version)"
archive="${repo_dir}/dist/trackkr-chrome-${version}.zip"
[[ -f "${archive}" ]] || fail "${archive} is missing"
unzip -tqq "${archive}" || fail "${archive} failed its integrity check"

# Structural validity is not the same as correct contents. A stale archive from
# an earlier build, or one modified after staging, passes unzip -t while
# shipping different files -- and the archive is what actually gets loaded.
extracted="$(mktemp -d)"
trap 'rm -rf "${extracted}"' EXIT
unzip -qq "${archive}" -d "${extracted}"

archived="$(cd "${extracted}" && find . -type f | sed 's|^\./||' | sort)"
if [[ "${archived}" != "${want}" ]]; then
  echo "archive contents differ from the allowlist:" >&2
  diff <(printf '%s\n' "${want}") <(printf '%s\n' "${archived}") >&2 || true
  exit 1
fi

# Same names is not the same bytes.
while IFS= read -r file; do
  if ! cmp -s "${stage_dir}/${file}" "${extracted}/${file}"; then
    fail "${file} differs between dist/chrome and the archive"
  fi
done <<< "${want}"

echo "chrome package valid: ${#expected[@]} files, archive ${archive}"
