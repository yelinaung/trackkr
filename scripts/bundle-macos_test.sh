#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=bundle-install.sh
source "${script_dir}/bundle-install.sh"

test_root="$(mktemp -d "${TMPDIR:-/tmp}/trackkr-bundle-test.XXXXXX")"
cleanup_test() {
  rm -rf "${test_root}"
}
trap cleanup_test EXIT

fail() {
  echo "bundle install test failed: $*" >&2
  exit 1
}

make_bundle() {
  local path="$1"
  local marker="$2"

  mkdir -p "${path}"
  printf '%s\n' "${marker}" >"${path}/marker"
}

assert_marker() {
  local path="$1"
  local want="$2"
  local got

  got="$(<"${path}/marker")"
  [[ "${got}" == "${want}" ]] || fail "${path} marker = ${got}, want ${want}"
}

assert_no_backups() {
  local applications_dir="$1"

  if compgen -G "${applications_dir}/.trackkr-backup.*" >/dev/null; then
    fail "${applications_dir} retained a backup directory"
  fi
}

fresh_apps="${test_root}/fresh/Applications"
fresh_staged="${fresh_apps}/.stage/trackkr.app"
fresh_bundle="${fresh_apps}/trackkr.app"
mkdir -p "${fresh_apps}"
make_bundle "${fresh_staged}" "new"
trackkr_install_bundle "${fresh_apps}" "${fresh_staged}" "${fresh_bundle}"
assert_marker "${fresh_bundle}" "new"
trackkr_commit_bundle
assert_no_backups "${fresh_apps}"

replace_apps="${test_root}/replace/Applications"
replace_staged="${replace_apps}/.stage/trackkr.app"
replace_bundle="${replace_apps}/trackkr.app"
mkdir -p "${replace_apps}"
make_bundle "${replace_bundle}" "old"
make_bundle "${replace_staged}" "new"
trackkr_install_bundle "${replace_apps}" "${replace_staged}" "${replace_bundle}"
assert_marker "${replace_bundle}" "new"
assert_marker "${trackkr_backup_bundle}" "old"
trackkr_commit_bundle
[[ -z "${trackkr_backup_bundle}" ]] || fail "successful replacement retained its backup"
assert_no_backups "${replace_apps}"

commit_failure_apps="${test_root}/commit-failure/Applications"
commit_failure_staged="${commit_failure_apps}/.stage/trackkr.app"
commit_failure_bundle="${commit_failure_apps}/trackkr.app"
mkdir -p "${commit_failure_apps}"
make_bundle "${commit_failure_bundle}" "old"
make_bundle "${commit_failure_staged}" "new"
trackkr_install_bundle \
  "${commit_failure_apps}" "${commit_failure_staged}" "${commit_failure_bundle}"
commit_failure_backup_dir="${trackkr_backup_dir}"
commit_failure_backup_bundle="${trackkr_backup_bundle}"

trackkr_remove_path() {
  if [[ "$1" == "${commit_failure_backup_dir}" ]]; then
    return 74
  fi
  command rm -rf "$@"
}

if trackkr_commit_bundle; then
  fail "forced commit cleanup failure returned success"
else
  commit_status=$?
fi
[[ "${commit_status}" -eq 74 ]] || fail "commit status = ${commit_status}, want 74"
[[ -z "${trackkr_backup_dir}" ]] || fail "failed commit retained its backup directory metadata"
[[ -z "${trackkr_backup_bundle}" ]] || fail "failed commit retained its backup bundle metadata"
assert_marker "${commit_failure_bundle}" "new"
assert_marker "${commit_failure_backup_bundle}" "old"

# Exercise the bundle script's EXIT cleanup path after the commit failure.
trackkr_restore_bundle "${commit_failure_bundle}"
assert_marker "${commit_failure_bundle}" "new"
assert_marker "${commit_failure_backup_bundle}" "old"
command rm -rf "${commit_failure_backup_dir}"
assert_no_backups "${commit_failure_apps}"

interrupt_apps="${test_root}/commit-interrupt/Applications"
interrupt_staged="${interrupt_apps}/.stage/trackkr.app"
interrupt_bundle="${interrupt_apps}/trackkr.app"
mkdir -p "${interrupt_apps}"
make_bundle "${interrupt_bundle}" "old"
make_bundle "${interrupt_staged}" "new"
trackkr_install_bundle "${interrupt_apps}" "${interrupt_staged}" "${interrupt_bundle}"
interrupt_backup_dir="${trackkr_backup_dir}"
interrupt_backup_bundle="${trackkr_backup_bundle}"

trackkr_remove_path() {
  kill -INT "${BASHPID}"
}

if (
  trap 'trackkr_restore_bundle "${interrupt_bundle}"; exit 130' INT
  trackkr_commit_bundle
); then
  fail "interrupted commit returned success"
else
  interrupt_status=$?
fi
[[ "${interrupt_status}" -eq 130 ]] || fail "interrupt status = ${interrupt_status}, want 130"
assert_marker "${interrupt_bundle}" "new"
assert_marker "${interrupt_backup_bundle}" "old"
trackkr_backup_dir=""
trackkr_backup_bundle=""
command rm -rf "${interrupt_backup_dir}"
assert_no_backups "${interrupt_apps}"

failure_apps="${test_root}/failure/Applications"
failure_staged="${failure_apps}/.stage/trackkr.app"
failure_bundle="${failure_apps}/trackkr.app"
mkdir -p "${failure_apps}"
make_bundle "${failure_bundle}" "old"
make_bundle "${failure_staged}" "new"

failed_source="${failure_staged}"
failure_log="${test_root}/failure.log"
trackkr_move_path() {
  if [[ "$1" == "${failed_source}" ]]; then
    return 73
  fi
  command mv "$@"
}

if trackkr_install_bundle \
  "${failure_apps}" "${failure_staged}" "${failure_bundle}" 2>"${failure_log}"; then
  fail "forced replacement failure returned success"
else
  install_status=$?
fi
[[ "${install_status}" -eq 73 ]] || fail "replacement status = ${install_status}, want 73"
assert_marker "${failure_bundle}" "old"
assert_marker "${failure_staged}" "new"
[[ -z "${trackkr_backup_bundle}" ]] || fail "restored replacement retained its backup"
assert_no_backups "${failure_apps}"
grep -F "Restored the previous" "${failure_log}" >/dev/null || fail "restoration was not reported"

echo "bundle install tests passed"
