//! Shared mobilebackup2 connection, cancellation and verdict handling.

use idevice::provider::IdeviceProvider;
use idevice::services::lockdown::LockdownClient;
use idevice::services::mobilebackup2::MobileBackup2Client;
use idevice::services::notification_proxy::NotificationProxyClient;
use idevice::IdeviceService;
use tokio_util::sync::CancellationToken;
use tracing::Instrument;

use crate::bounded;
use crate::engine_error::{EngineFailure, ErrorKind};
use crate::provider::AirvaultProvider;
use crate::timeouts;

/// Connect with the pairing record's escrow bag so backupd can read protected
/// keychain items while the phone is locked. `udid` is the device's own, sent
/// as the requests' TargetIdentifier.
pub(crate) async fn connect(
    provider: &AirvaultProvider,
    udid: &str,
) -> Result<MobileBackup2Client, idevice::IdeviceError> {
    let pairing = provider.get_pairing_file().await?;
    let mut lockdown = LockdownClient::connect(provider).await?;
    let legacy = lockdown.start_session(&pairing).await?;
    let service = MobileBackup2Client::service_name();
    let (port, ssl) = match lockdown
        .start_service_with_escrow(service.clone(), pairing.escrow_bag.clone())
        .await
    {
        Ok(result) => result,
        Err(error) => {
            // A stale escrow bag must not block backups of an unlocked phone.
            tracing::warn!(?error, "mb2: escrow StartService rejected, retrying plain");
            lockdown.start_service(service).await?
        }
    };
    let mut device = provider.connect(port).await?;
    if ssl {
        device.start_session(&pairing, legacy).await?;
    }
    device.set_udid(udid);
    MobileBackup2Client::from_stream(device).await
}

pub(crate) async fn disconnect_bounded(
    client: &mut MobileBackup2Client,
    operation: &str,
) -> Result<(), String> {
    bounded::step(
        timeouts::TEARDOWN,
        &format!("{operation} mobilebackup2 disconnect"),
        client.disconnect(),
    )
    .await
}

/// Watches for a device-initiated sync abort and forwards it to the transfer's
/// cancellation token.
pub(crate) async fn spawn_cancel_observer(
    provider: &AirvaultProvider,
    cancel: CancellationToken,
) -> Result<tokio::task::JoinHandle<()>, idevice::IdeviceError> {
    const CANCEL: &str = "com.apple.itunes-client.syncCancelRequest";

    let mut notifications = NotificationProxyClient::connect(provider).await?;
    notifications.observe_notification(CANCEL).await?;

    let span = tracing::Span::current();
    Ok(tokio::spawn(
        async move {
            loop {
                match notifications.receive_notification().await {
                    Ok(name) if name.contains("syncCancelRequest") => {
                        tracing::info!("mb2: device requested cancel");
                        cancel.cancel();
                        return;
                    }
                    Ok(_) => {}
                    Err(_) => return,
                }
            }
        }
        .instrument(span),
    ))
}

#[derive(Debug)]
pub(crate) struct VerdictError {
    pub(crate) code: Option<i128>,
    pub(crate) detail: String,
}

impl From<VerdictError> for EngineFailure {
    fn from(error: VerdictError) -> Self {
        let kind = match error.code {
            // MBErrorDomain: invalid password, protected data unavailable, Find My.
            Some(207) => ErrorKind::InvalidBackupPassword,
            Some(208) => ErrorKind::DeviceLocked,
            Some(211) => ErrorKind::FindMyEnabled,
            _ => ErrorKind::Protocol,
        };
        Self::new(kind, error.detail)
    }
}

/// Classifies a backup or restore verdict. In a backup, 208 is the passcode
/// prompt iOS raises before every host backup — dismissed or left to time out —
/// not a locked-phone refusal, so it gets its own kind.
pub(crate) fn transfer_verdict_failure(error: VerdictError, backup: bool) -> EngineFailure {
    if backup && error.code == Some(208) {
        return EngineFailure::new(ErrorKind::BackupNotConfirmed, error.detail);
    }
    EngineFailure::from(error)
}

/// Extract a structured final DeviceLink verdict without parsing localized
/// error text.
pub(crate) fn verdict(
    outcome: Option<plist::Dictionary>,
    fallback: &str,
) -> Result<(), VerdictError> {
    let Some(dictionary) = outcome else {
        return Err(VerdictError {
            code: None,
            detail: format!("{fallback}: missing final device verdict"),
        });
    };
    let Some(value) = dictionary.get("ErrorCode") else {
        return Err(VerdictError {
            code: None,
            detail: format!("{fallback}: final device verdict is missing ErrorCode"),
        });
    };
    let code = value
        .as_signed_integer()
        .map(i128::from)
        .or_else(|| value.as_unsigned_integer().map(i128::from));
    let code = match code {
        Some(0) => return Ok(()),
        Some(code) => code,
        None => {
            return Err(VerdictError {
                code: None,
                detail: format!("{fallback}: final device verdict has malformed ErrorCode"),
            })
        }
    };
    let description = dictionary
        .get("ErrorDescription")
        .and_then(|value| value.as_string())
        .unwrap_or(fallback);
    Err(VerdictError {
        code: Some(code),
        detail: format!("{description} (code {code})"),
    })
}

#[cfg(test)]
mod tests {
    use plist::{Dictionary, Value};

    use super::{transfer_verdict_failure, verdict};
    use crate::engine_error::ErrorKind;

    fn device_verdict(code: i64) -> Option<Dictionary> {
        let mut value = Dictionary::new();
        value.insert("ErrorCode".into(), Value::Integer(code.into()));
        value.insert(
            "ErrorDescription".into(),
            Value::String("localized device detail".into()),
        );
        Some(value)
    }

    #[test]
    fn known_verdict_codes_have_stable_kinds() {
        // 208 is the unanswered passcode prompt only in a backup verdict.
        let cases = [
            (207, false, ErrorKind::InvalidBackupPassword),
            (208, false, ErrorKind::DeviceLocked),
            (208, true, ErrorKind::BackupNotConfirmed),
            (211, false, ErrorKind::FindMyEnabled),
        ];
        for (code, backup, expected) in cases {
            let error = verdict(device_verdict(code), "transfer failed").unwrap_err();
            assert_eq!(transfer_verdict_failure(error, backup).kind, expected);
        }
    }
}
