// User-facing error catalogue. Transport and service layers exchange stable
// codes only; keeping copy here makes another locale a catalogue swap instead
// of a backend/API change.
const english = {
  unknown_error: 'Something went wrong',
  network_error: 'Cannot reach the AirVault backend',
  invalid_response: 'The backend returned an invalid response',

  devices_load_failed: 'Could not load devices',
  device_load_failed: 'Could not load the device',
  sign_in_failed: 'Sign-in failed',
  current_backup_password_required: 'Enter the current backup password',
  new_backup_password_required: 'Enter a new backup password',
  backup_passwords_do_not_match: 'Passwords do not match',

  bad_request: 'The request was not valid',
  authentication_required: 'That Web UI token is not valid',
  forbidden: 'You do not have permission to do that',
  not_found: 'The requested item was not found',
  method_not_allowed: 'That action is not supported',
  payload_too_large: 'The uploaded file is too large',
  unsupported_media_type: 'That file type is not supported',
  invalid_udid: 'The device identifier is not valid',
  invalid_power_action: 'Choose restart, shutdown, or sleep',
  bundle_id_required: 'An application identifier is required',
  invalid_bundle_id: 'The application identifier is not valid',
  paths_required: 'Select at least one file',
  invalid_path: 'The file path is not valid',
  too_many_paths: 'Too many files were requested at once',
  invalid_gallery_page: 'The requested gallery page is not valid',
  file_required: 'Select a file',
  invalid_thumbnail_path: 'The thumbnail path is not valid',
  ipa_required: 'Choose an .ipa file to install',
  invalid_ipa: 'The selected file must be an .ipa',
  backup_password_change_empty: 'Enter the old and/or new backup password',
  snapshot_required: 'Select a restore point',
  snapshot_not_found: 'The selected restore point no longer exists',
  backup_not_restorable: 'The selected backup is not confirmed restorable',
  backup_password_required: 'This backup is encrypted; enter its password',
  backup_ios_too_new: 'This backup needs a newer iOS; update the phone first',
  upstream_error: 'A required service did not respond',
  service_unavailable: 'The service is temporarily unavailable',
  internal_error: 'AirVault could not complete the request',

  device_offline: 'The device is offline',
  device_never_came_online:
    'The phone never came online — wake it or connect it by USB, then back up again',
  device_locked: 'Unlock the iPhone and try again',
  backup_not_confirmed:
    "The iPhone didn't confirm the backup — enter the passcode on the phone when it asks (the prompt closes after about a minute)",
  find_my_enabled: 'Turn off Find My iPhone on the phone, then restore again',
  activation_lock: 'Activation Lock is on — sign out of the linked Apple Account on this phone, then restore again',
  activation_failed: 'Could not activate the phone with Apple — check the server internet access and try again',
  device_timeout: 'The iPhone did not respond in time',
  device_connection_interrupted: 'The connection to the iPhone was interrupted; reconnect it and try again',
  device_action_failed: 'The device action failed',
  power_request_failed: 'The power request failed',
  sleep_request_failed: 'The sleep request failed',
  pairing_required: 'Pair the phone again before continuing',
  pairing_cleanup_failed: 'Could not remove the local pairing data; check the lockdown volume permissions',
  resource_busy: 'Another exclusive operation is using this device or backup',
  operation_cancelled: 'The operation was cancelled',
  operation_state_conflict: 'The operation has already moved to another state',

  pair_state_failed: 'Could not read the pairing state',
  pairing_failed: 'Pairing failed',
  wifi_authorization_failed:
    "AirVault couldn't finish Wi-Fi setup. Keep the cable connected and try again",
  pairing_registration_failed: 'The phone trusted AirVault, but registration failed',
  pairing_timeout: 'Pairing timed out; start it again when the phone is ready',
  hardware_query_failed: 'Failed to read hardware information',
  battery_query_failed: 'Failed to read the battery state',
  app_list_failed: 'Failed to list applications',
  app_install_failed: 'The application could not be installed',
  app_uninstall_failed: 'The application could not be removed',
  app_files_failed: "Could not read the application's files",
  app_file_delete_failed: 'The file could not be deleted',
  file_list_failed: 'Could not read the files',
  file_delete_failed: 'Delete failed',
  media_list_failed: 'Could not read files from the phone',
  gallery_failed: 'Could not read the gallery',
  gallery_revision_changed: 'The gallery changed; reload it from the first page',
  stat_failed: 'Could not read the file information',
  thumb_failed: 'Could not read the gallery thumbnails',
  download_failed: 'The file could not be downloaded',
  console_stream_failed: 'The device log stream stopped',
  backup_password_change_failed: 'The backup password could not be changed',
  auto_backup_save_failed: 'The automatic backup settings could not be saved',
  invalid_auto_backup_interval: 'Choose how often to back up',
  invalid_auto_backup_window: 'Choose two different times for the window',
  invalid_time_zone: "This browser's time zone is not recognized by the server",

  backup_start_failed: 'Failed to start backup',
  backup_cancel_failed: 'Failed to cancel',
  backup_history_failed: 'Could not load the restore points',
  snapshot_delete_failed: 'Could not delete the snapshot',
  backup_failed: 'The backup failed',
  restore_start_failed: 'The restore could not be started',
  restore_failed: 'The restore failed',
  invalid_backup_password: 'The backup password is incorrect',
  operation_outcome_unknown: 'The iPhone may have applied the password change, but its final state could not be confirmed',
  storage_full: 'The backup disk is full; free up space and try again',
  backup_integrity_failed: 'Stored backup data failed an integrity check; check disk health and the AirVault log',
} as const satisfies Record<string, string>;

/** Keys owned by the UI. API codes remain strings because a newer backend can
 * legitimately send a code that an older SPA does not know yet. */
export type ErrorTextKey = keyof typeof english;

const reportedUnknownCodes = new Set<string>();

function isErrorTextKey(code: string): code is ErrorTextKey {
  return Object.prototype.hasOwnProperty.call(english, code);
}

function reportUnknownCode(code: string): void {
  if (reportedUnknownCodes.has(code)) return;
  reportedUnknownCodes.add(code);
  console.warn(`Unknown error code from API: ${code}`);
}

export function errorText(code: string, fallbackCode: ErrorTextKey = 'unknown_error'): string {
  if (isErrorTextKey(code)) return english[code];
  reportUnknownCode(code);
  return english[fallbackCode];
}
