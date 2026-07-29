#!/usr/bin/env bash
set -euo pipefail

if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "bundle-macos.sh must run on macOS" >&2
  exit 1
fi

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_dir="$(cd "${script_dir}/.." && pwd)"
user_home="${HOME:?HOME must be set}"
# shellcheck source=bundle-install.sh
source "${script_dir}/bundle-install.sh"

applications_dir="${user_home}/Applications"
bundle_path="${applications_dir}/trackkr.app"
executable_path="${bundle_path}/Contents/MacOS/trackkrd"
launch_agents_dir="${user_home}/Library/LaunchAgents"
agent_path="${launch_agents_dir}/com.trackkr.daemon.plist"
config_dir="${user_home}/Library/Application Support/trackkr"
config_path="${config_dir}/config.toml"
logs_dir="${user_home}/Library/Logs/trackkr"

stage_dir=""
agent_tmp=""
cleanup() {
  exit_status=$?
  trap - EXIT

  if ! trackkr_restore_bundle "${bundle_path}"; then
    exit_status=1
  fi
  if [[ -n "${stage_dir}" ]]; then
    rm -rf "${stage_dir}"
  fi
  if [[ -n "${agent_tmp}" ]]; then
    rm -f "${agent_tmp}"
  fi
  exit "${exit_status}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

mkdir -p "${applications_dir}" "${launch_agents_dir}" "${config_dir}" "${logs_dir}"
# Keep staging on the destination filesystem so installation is a rename.
stage_dir="$(mktemp -d "${applications_dir}/.trackkr-stage.XXXXXX")"

staged_bundle="${stage_dir}/trackkr.app"
mkdir -p "${staged_bundle}/Contents/MacOS"
cp "${repo_dir}/deploy/Info.plist" "${staged_bundle}/Contents/Info.plist"

(
  cd "${repo_dir}"
  go build -trimpath -o "${staged_bundle}/Contents/MacOS/trackkrd" ./cmd/trackkrd
)

sign_identity="${TRACKKR_SIGN_IDENTITY:-}"
if [[ -n "${sign_identity}" ]]; then
  codesign --force --sign "${sign_identity}" "${staged_bundle}"
else
  echo "warning: TRACKKR_SIGN_IDENTITY is unset; using ad-hoc signing" >&2
  echo "warning: macOS may require Accessibility permission again after each rebuild" >&2
  codesign --force --sign - "${staged_bundle}"
fi
codesign --verify --strict "${staged_bundle}"

# Retain the working bundle until the replacement rename succeeds. The
# exit trap restores it if the installation stops in between the renames.
trackkr_install_bundle "${applications_dir}" "${staged_bundle}" "${bundle_path}"

agent_tmp="$(mktemp "${launch_agents_dir}/.trackkr-agent.XXXXXX")"
escape_xml() {
  printf '%s' "$1" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g'
}
executable_xml="$(escape_xml "${executable_path}")"
config_xml="$(escape_xml "${config_path}")"
logs_xml="$(escape_xml "${logs_dir}")"
cat >"${agent_tmp}" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.trackkr.daemon</string>
    <key>ProgramArguments</key>
    <array>
        <string>${executable_xml}</string>
        <string>-config</string>
        <string>${config_xml}</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>${logs_xml}/daemon.log</string>
    <key>StandardErrorPath</key>
    <string>${logs_xml}/daemon.err.log</string>
</dict>
</plist>
PLIST
plutil -lint "${agent_tmp}" >/dev/null
chmod 644 "${agent_tmp}"
mv "${agent_tmp}" "${agent_path}"
agent_tmp=""

trackkr_commit_bundle

echo "Installed ${bundle_path}"
echo "Wrote ${agent_path}"
if [[ ! -f "${config_path}" ]]; then
  echo "Create ${config_path} before loading the agent"
fi
