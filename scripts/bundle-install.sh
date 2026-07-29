#!/usr/bin/env bash

trackkr_backup_dir=""
trackkr_backup_bundle=""

trackkr_move_path() {
  mv "$@"
}

trackkr_remove_path() {
  rm -rf "$@"
}

trackkr_restore_bundle() {
  local bundle_path="$1"

  if [[ -z "${trackkr_backup_bundle}" ||
    ( ! -e "${trackkr_backup_bundle}" && ! -L "${trackkr_backup_bundle}" ) ]]; then
    return 0
  fi

  if rm -rf "${bundle_path}" && trackkr_move_path "${trackkr_backup_bundle}" "${bundle_path}"; then
    echo "Restored the previous ${bundle_path}" >&2
    rmdir "${trackkr_backup_dir}" 2>/dev/null || true
    trackkr_backup_dir=""
    trackkr_backup_bundle=""
    return 0
  fi

  echo "error: could not restore the previous bundle; it remains at ${trackkr_backup_bundle}" >&2
  return 1
}

trackkr_install_bundle() {
  local applications_dir="$1"
  local staged_bundle="$2"
  local bundle_path="$3"
  local move_status

  trackkr_backup_dir=""
  trackkr_backup_bundle=""
  if [[ -e "${bundle_path}" || -L "${bundle_path}" ]]; then
    trackkr_backup_dir="$(mktemp -d "${applications_dir}/.trackkr-backup.XXXXXX")"
    trackkr_backup_bundle="${trackkr_backup_dir}/trackkr.app"
    if trackkr_move_path "${bundle_path}" "${trackkr_backup_bundle}"; then
      :
    else
      move_status=$?
      rmdir "${trackkr_backup_dir}" 2>/dev/null || true
      trackkr_backup_dir=""
      trackkr_backup_bundle=""
      return "${move_status}"
    fi
  fi

  if trackkr_move_path "${staged_bundle}" "${bundle_path}"; then
    return 0
  else
    move_status=$?
  fi

  echo "error: could not install ${bundle_path}" >&2
  if ! trackkr_restore_bundle "${bundle_path}"; then
    return 1
  fi
  return "${move_status}"
}

trackkr_commit_bundle() {
  local committed_backup_dir="${trackkr_backup_dir}"

  # The replacement is already valid. Invalidate rollback state before
  # deleting its obsolete backup so cleanup cannot restore a partial copy.
  trackkr_backup_bundle=""
  trackkr_backup_dir=""
  if [[ -n "${committed_backup_dir}" ]]; then
    trackkr_remove_path "${committed_backup_dir}"
  fi
}
