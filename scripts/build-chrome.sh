#!/usr/bin/env bash
# Stage and package the Chrome extension.
#
# The staging directory is deleted and recreated, and files are copied from an
# explicit allowlist rather than excluded by pattern. A pattern-based build
# quietly ships whatever it forgot to exclude -- test fixtures, a stray Firefox
# manifest, an editor backup -- and the package is the one artifact nobody reads
# before loading it.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_dir="$(cd "${script_dir}/.." && pwd)"
source_dir="${repo_dir}/extension"
stage_dir="${repo_dir}/dist/chrome"

# Every runtime file, named. background.js is gone; the Firefox entrypoint and
# manifest are deliberately absent.
runtime_files=(
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
)
icon_files=(
  icons/icon-48.png
  icons/icon-96.png
)

version="$(
  python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["version"])' \
    "${source_dir}/manifest.chrome.json"
)"
if [[ -z "${version}" ]]; then
  echo "error: manifest.chrome.json has no version" >&2
  exit 1
fi

rm -rf "${stage_dir}"
mkdir -p "${stage_dir}/icons"

cp "${source_dir}/manifest.chrome.json" "${stage_dir}/manifest.json"
for file in "${runtime_files[@]}" "${icon_files[@]}"; do
  if [[ ! -f "${source_dir}/${file}" ]]; then
    echo "error: ${file} is referenced by the build but missing from extension/" >&2
    exit 1
  fi
  cp "${source_dir}/${file}" "${stage_dir}/${file}"
done

# A Gecko-only key in the Chrome manifest loads with a warning and drifts from
# there, so fail instead.
if grep -q 'browser_specific_settings' "${stage_dir}/manifest.json"; then
  echo "error: the Chrome manifest carries Gecko keys" >&2
  exit 1
fi

archive="${repo_dir}/dist/trackkr-chrome-${version}.zip"
rm -f "${archive}"
(cd "${stage_dir}" && zip -q -r -X "${archive}" .)

# An archive nothing can read is worse than no archive: verify before claiming
# success.
if ! unzip -tqq "${archive}"; then
  echo "error: ${archive} failed its integrity check" >&2
  exit 1
fi

echo "Staged ${stage_dir}"
echo "Packaged ${archive}"
