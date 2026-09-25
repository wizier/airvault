//! Internal, typed error classification for device operations.
//!
//! `ErrorKind` is the only way Rust names a failure, and `.code()` at an export
//! the only way one reaches the ABI — a stream/status integer can never become
//! an engine error. The `AV_ERROR_*` constants set the discriminant values.

use idevice::services::afc::errors::AfcError;
use idevice::usbmuxd::errors::UsbmuxdError;
use idevice::IdeviceError;

use crate::ffi;
use crate::object_store::{ObjectFailure, ObjectFailureKind};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
#[repr(i32)]
pub(crate) enum ErrorKind {
    InvalidArgument = ffi::AV_ERROR_INVALID_ARGUMENT,
    DeviceUnavailable = ffi::AV_ERROR_DEVICE_UNAVAILABLE,
    DeviceLocked = ffi::AV_ERROR_DEVICE_LOCKED,
    TrustRequired = ffi::AV_ERROR_TRUST_REQUIRED,
    UserDenied = ffi::AV_ERROR_USER_DENIED,
    Busy = ffi::AV_ERROR_BUSY,
    Timeout = ffi::AV_ERROR_TIMEOUT,
    Cancelled = ffi::AV_ERROR_CANCELLED,
    Protocol = ffi::AV_ERROR_PROTOCOL,
    StorageFull = ffi::AV_ERROR_STORAGE_FULL,
    Integrity = ffi::AV_ERROR_INTEGRITY,
    Unsupported = ffi::AV_ERROR_UNSUPPORTED,
    Internal = ffi::AV_ERROR_INTERNAL,
    InvalidBackupPassword = ffi::AV_ERROR_INVALID_BACKUP_PASSWORD,
    OutcomeUnknown = ffi::AV_ERROR_OUTCOME_UNKNOWN,
    FindMyEnabled = ffi::AV_ERROR_FIND_MY_ENABLED,
}

impl ErrorKind {
    pub(crate) const fn code(self) -> i32 {
        self as i32
    }
}

/// An AirVault error category plus diagnostic detail. Category conversion to
/// the ABI's `i32` happens only when an exported function returns.
#[derive(Debug)]
pub(crate) struct EngineFailure {
    pub(crate) kind: ErrorKind,
    pub(crate) detail: String,
}

impl EngineFailure {
    pub(crate) fn new(kind: ErrorKind, detail: impl Into<String>) -> Self {
        Self {
            kind,
            detail: detail.into(),
        }
    }

    pub(crate) fn invalid_argument(detail: impl Into<String>) -> Self {
        Self::new(ErrorKind::InvalidArgument, detail)
    }

    pub(crate) fn integrity(detail: impl Into<String>) -> Self {
        Self::new(ErrorKind::Integrity, detail)
    }

    pub(crate) fn from_idevice(context: &str, error: IdeviceError) -> Self {
        let kind = classify_idevice_error(&error);
        Self::new(kind, format!("{context}: {error} [{error:?}]"))
    }

    /// A connection failure before an operation starts means "offline".
    /// Losing that connection during DeviceLink means an interrupted protocol.
    pub(crate) fn from_active_transfer(context: &str, error: IdeviceError) -> Self {
        let kind = match classify_idevice_error(&error) {
            ErrorKind::DeviceUnavailable => ErrorKind::Protocol,
            kind => kind,
        };
        Self::new(kind, format!("{context}: {error} [{error:?}]"))
    }

    /// A single request/response call. Protocol, Integrity and StorageFull
    /// describe a DeviceLink transfer or the backup store; for one request they
    /// would mislead, so Go reports the operation's own failure instead.
    pub(crate) fn from_request(context: &str, error: IdeviceError) -> Self {
        let kind = match classify_idevice_error(&error) {
            ErrorKind::Protocol | ErrorKind::Integrity | ErrorKind::StorageFull => {
                ErrorKind::Internal
            }
            kind => kind,
        };
        Self::new(kind, format!("{context}: {error} [{error:?}]"))
    }

    pub(crate) fn with_cleanup(mut self, cleanup: String) -> Self {
        self.detail.push_str("; cleanup also failed: ");
        self.detail.push_str(&cleanup);
        self
    }
}

impl From<IdeviceError> for EngineFailure {
    fn from(error: IdeviceError) -> Self {
        Self::from_request("device request failed", error)
    }
}

impl From<ObjectFailure> for EngineFailure {
    fn from(error: ObjectFailure) -> Self {
        let kind = match error.kind {
            ObjectFailureKind::StorageFull => ErrorKind::StorageFull,
            ObjectFailureKind::Integrity => ErrorKind::Integrity,
            ObjectFailureKind::Internal => ErrorKind::Internal,
        };
        Self::new(kind, error.detail)
    }
}

fn classify_afc_error(error: &AfcError) -> ErrorKind {
    match error {
        AfcError::OpTimeout => ErrorKind::Timeout,
        AfcError::ServiceNotConnected | AfcError::MuxError => ErrorKind::DeviceUnavailable,
        AfcError::ObjectBusy | AfcError::OpWouldBlock | AfcError::OpInProgress => ErrorKind::Busy,
        AfcError::NoSpaceLeft => ErrorKind::StorageFull,
        AfcError::ObjectNotFound => ErrorKind::Integrity,
        AfcError::OpNotSupported => ErrorKind::Unsupported,
        _ => ErrorKind::Protocol,
    }
}

fn classify_idevice_error(error: &IdeviceError) -> ErrorKind {
    match error {
        IdeviceError::Afc(error) => classify_afc_error(error),
        IdeviceError::Timeout => ErrorKind::Timeout,

        IdeviceError::Socket(_)
        | IdeviceError::DeviceNotFound
        | IdeviceError::ServiceNotFound
        | IdeviceError::NoEstablishedConnection
        | IdeviceError::Usbmuxd(UsbmuxdError::ConnectionRefused | UsbmuxdError::BadDevice) => {
            ErrorKind::DeviceUnavailable
        }

        IdeviceError::DeviceLocked
        | IdeviceError::PasswordProtected
        | IdeviceError::GetProhibited => ErrorKind::DeviceLocked,

        IdeviceError::InvalidHostID
        | IdeviceError::SessionInactive
        | IdeviceError::PairingDialogResponsePending => ErrorKind::TrustRequired,

        IdeviceError::UserDeniedPairing => ErrorKind::UserDenied,
        IdeviceError::CanceledByUser => ErrorKind::Cancelled,
        IdeviceError::NotFound | IdeviceError::BadBuildManifest => ErrorKind::Integrity,
        IdeviceError::DeveloperModeNotEnabled => ErrorKind::Unsupported,
        IdeviceError::FfiInvalidArg
        | IdeviceError::FfiInvalidString
        | IdeviceError::InvalidArgument => ErrorKind::InvalidArgument,

        // IdeviceError is non-exhaustive. Unknown device/protocol failures
        // retain their full diagnostic text and use the conservative category.
        _ => ErrorKind::Protocol,
    }
}

#[cfg(test)]
mod tests {
    use std::io;

    use super::{EngineFailure, ErrorKind};
    use crate::ffi;
    use idevice::services::afc::errors::AfcError;
    use idevice::usbmuxd::errors::UsbmuxdError;
    use idevice::IdeviceError;

    /// Success and the pull-stream rc values must stay outside the error space.
    #[test]
    fn reserved_rc_values_are_not_error_kinds() {
        for reserved in [0, ffi::AV_STREAM_CONTINUE, ffi::AV_STREAM_CLOSED] {
            assert!(
                !(ffi::AV_ERROR_INVALID_ARGUMENT..=ffi::AV_ERROR_FIND_MY_ENABLED)
                    .contains(&reserved),
                "{reserved}"
            );
        }
    }

    #[test]
    fn representative_upstream_errors_have_stable_kinds() {
        let cases = [
            (IdeviceError::Timeout, ErrorKind::Timeout),
            (
                IdeviceError::Afc(AfcError::NoSpaceLeft),
                ErrorKind::StorageFull,
            ),
            (IdeviceError::Afc(AfcError::ObjectBusy), ErrorKind::Busy),
            (IdeviceError::InvalidHostID, ErrorKind::TrustRequired),
            (
                IdeviceError::Usbmuxd(UsbmuxdError::ConnectionRefused),
                ErrorKind::DeviceUnavailable,
            ),
            (
                IdeviceError::Usbmuxd(UsbmuxdError::BadCommand),
                ErrorKind::Protocol,
            ),
        ];
        for (error, expected) in cases {
            assert_eq!(
                EngineFailure::from_idevice("operation", error).kind,
                expected
            );
        }
    }

    #[test]
    fn requests_keep_device_states_but_not_transfer_kinds() {
        let cases = [
            (IdeviceError::DeviceLocked, ErrorKind::DeviceLocked),
            (IdeviceError::InvalidHostID, ErrorKind::TrustRequired),
            (IdeviceError::Timeout, ErrorKind::Timeout),
            (
                IdeviceError::Afc(AfcError::ObjectNotFound),
                ErrorKind::Internal,
            ),
            (
                IdeviceError::UnexpectedResponse("refused".into()),
                ErrorKind::Internal,
            ),
        ];
        for (error, expected) in cases {
            assert_eq!(EngineFailure::from(error).kind, expected);
        }
    }

    #[test]
    fn connection_loss_changes_kind_after_transfer_starts() {
        let socket_error =
            || IdeviceError::Socket(io::Error::new(io::ErrorKind::BrokenPipe, "broken pipe"));
        assert_eq!(
            EngineFailure::from_idevice("connect", socket_error()).kind,
            ErrorKind::DeviceUnavailable
        );
        assert_eq!(
            EngineFailure::from_active_transfer("backup", socket_error()).kind,
            ErrorKind::Protocol
        );
    }
}
