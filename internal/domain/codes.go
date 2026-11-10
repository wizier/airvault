package domain

// Code names a failure to the SPA, which words it: every code needs a text in
// web/src/lib/error-text.ts.
type Code string

// What the HTTP layer answers on its own.
const (
	CodeBadRequest             Code = "bad_request"
	CodeAuthenticationRequired Code = "authentication_required"
	CodeForbidden              Code = "forbidden"
	CodeNotFound               Code = "not_found"
	CodeMethodNotAllowed       Code = "method_not_allowed"
	CodePayloadTooLarge        Code = "payload_too_large"
	CodeUnsupportedMediaType   Code = "unsupported_media_type"
	CodeInternalError          Code = "internal_error"
)

// The state of a device or an operation.
const (
	CodeDeviceOffline               Code = "device_offline"
	CodeDeviceNeverCameOnline       Code = "device_never_came_online"
	CodeDeviceLocked                Code = "device_locked"
	CodeDeviceTimeout               Code = "device_timeout"
	CodeDeviceStorageFull           Code = "device_storage_full"
	CodeDeviceConnectionInterrupted Code = "device_connection_interrupted"
	CodeFindMyEnabled               Code = "find_my_enabled"
	CodeStorageFull                 Code = "storage_full"
	CodePairingRequired             Code = "pairing_required"
	CodePairingCleanupFailed        Code = "pairing_cleanup_failed"
	CodeResourceBusy                Code = "resource_busy"
	CodeOperationCancelled          Code = "operation_cancelled"
	CodeOperationStateConflict      Code = "operation_state_conflict"
	CodeOperationOutcomeUnknown     Code = "operation_outcome_unknown"
)

// Input the server refuses.
const (
	CodeInvalidUDID               Code = "invalid_udid"
	CodeInvalidPath               Code = "invalid_path"
	CodeInvalidPage               Code = "invalid_page"
	CodeFileRequired              Code = "file_required"
	CodePathsRequired             Code = "paths_required"
	CodeTooManyPaths              Code = "too_many_paths"
	CodeInvalidThumbnailPath      Code = "invalid_thumbnail_path"
	CodeBundleIDRequired          Code = "bundle_id_required"
	CodeInvalidBundleID           Code = "invalid_bundle_id"
	CodeTooManyBundleIDs          Code = "too_many_bundle_ids"
	CodeIPARequired               Code = "ipa_required"
	CodeInvalidIPA                Code = "invalid_ipa"
	CodeInvalidPowerAction        Code = "invalid_power_action"
	CodeInvalidWallpaperScreen    Code = "invalid_wallpaper_screen"
	CodeInvalidAutoBackupInterval Code = "invalid_auto_backup_interval"
	CodeInvalidAutoVerifyInterval Code = "invalid_auto_verify_interval"
	CodeInvalidAutoWindow         Code = "invalid_auto_window"
	CodeInvalidCleanupDays        Code = "invalid_cleanup_days"
	CodeInvalidCleanupThin        Code = "invalid_cleanup_thin"
	CodeSnapshotRequired          Code = "snapshot_required"
	CodeSnapshotNotFound          Code = "snapshot_not_found"
	CodeBackupDamaged             Code = "backup_damaged"
	CodeBackupNotRestorable       Code = "backup_not_restorable"
	CodeBackupNotBrowsable        Code = "backup_not_browsable"
	CodeBackupLocked              Code = "backup_locked"
	CodeBackupFileNotStored       Code = "backup_file_not_stored"
	CodeBackupPasswordRequired    Code = "backup_password_required"
	CodeInvalidBackupPassword     Code = "invalid_backup_password"
	CodeBackupPasswordChangeEmpty Code = "backup_password_change_empty"
	CodeBackupIOSTooNew           Code = "backup_ios_too_new"
	CodeInvalidPhotoFilter        Code = "invalid_photo_filter"
	CodeChatRequired              Code = "chat_required"
	CodeQueryTooShort             Code = "query_too_short"
)

// A request that reached the device or the backup and failed there.
const (
	CodeHardwareQueryFailed        Code = "hardware_query_failed"
	CodeBatteryQueryFailed         Code = "battery_query_failed"
	CodePowerRequestFailed         Code = "power_request_failed"
	CodeSleepRequestFailed         Code = "sleep_request_failed"
	CodeEraseFailed                Code = "erase_failed"
	CodeConsoleStreamFailed        Code = "console_stream_failed"
	CodeAppListFailed              Code = "app_list_failed"
	CodeAppIconsFailed             Code = "app_icons_failed"
	CodeAppInstallFailed           Code = "app_install_failed"
	CodeAppUninstallFailed         Code = "app_uninstall_failed"
	CodeAppFilesFailed             Code = "app_files_failed"
	CodeAppFileDeleteFailed        Code = "app_file_delete_failed"
	CodeMediaListFailed            Code = "media_list_failed"
	CodeGalleryFailed              Code = "gallery_failed"
	CodeGalleryRevisionChanged     Code = "gallery_revision_changed"
	CodeThumbFailed                Code = "thumb_failed"
	CodeStatFailed                 Code = "stat_failed"
	CodeDownloadFailed             Code = "download_failed"
	CodeBackupPasswordChangeFailed Code = "backup_password_change_failed"
	CodePairStateFailed            Code = "pair_state_failed"
	CodePairingFailed              Code = "pairing_failed"
	CodePairingTimeout             Code = "pairing_timeout"
	CodePairingRegistrationFailed  Code = "pairing_registration_failed"
	CodeWiFiAuthorizationFailed    Code = "wifi_authorization_failed"
)

// How a backup, restore or integrity check failed.
const (
	CodeBackupFailed          Code = "backup_failed"
	CodeRestoreFailed         Code = "restore_failed"
	CodeVerifyFailed          Code = "verify_failed"
	CodeBackupNotConfirmed    Code = "backup_not_confirmed" // the passcode prompt was dismissed or timed out
	CodeBackupIntegrityFailed Code = "backup_integrity_failed"
	CodeActivationLock        Code = "activation_lock"
	CodeActivationFailed      Code = "activation_failed"
)
