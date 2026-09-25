//! Shared mobilebackup2 connection, cancellation and verdict handling.

use std::time::Duration;

use idevice::provider::IdeviceProvider;
use idevice::services::lockdown::LockdownClient;
use idevice::services::mobilebackup2::MobileBackup2Client;
use idevice::services::notification_proxy::NotificationProxyClient;
use idevice::IdeviceService;
use tokio_util::sync::CancellationToken;
use tracing::Instrument;

use crate::bounded;
use crate::engine_error::{EngineFailure, ErrorKind};
use crate::AirvaultProvider;

const DISCONNECT_TIMEOUT: Duration = crate::timeouts::TEARDOWN;

/// Connect with the pairing record's escrow bag so backupd can read protected
/// keychain items while the phone is locked.
pub(crate) async fn connect(
    provider: &AirvaultProvider,
) -> Result<MobileBackup2Client, idevice::IdeviceError> {
    let pairing = provider.get_pairing_file().await?;
    let mut lockdown = LockdownClient::connect(provider).await?;
    let legacy = lockdown.start_session(&pairing).await?;
    let udid = lockdown
        .get_value(Some("UniqueDeviceID"), None)
        .await
        .ok()
        .and_then(|value| value.as_string().map(str::to_owned));
    if udid.is_none() {
        tracing::warn!("mb2: UniqueDeviceID unavailable; using default Target/Source identifiers");
    }

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
    if let Some(udid) = udid {
        device.set_udid(udid);
    }
    MobileBackup2Client::from_stream(device).await
}

pub(crate) async fn disconnect_bounded(
    client: &mut MobileBackup2Client,
    operation: &str,
) -> Result<(), String> {
    bounded::step(
        DISCONNECT_TIMEOUT,
        &format!("{operation} mobilebackup2 disconnect"),
        client.disconnect(),
    )
    .await
}

/// Watches for a device-initiated sync abort and forwards it to the transfer's
/// cancellation token. Failure to create the optional observer is non-fatal.
pub(crate) async fn spawn_cancel_observer(
    provider: &AirvaultProvider,
    cancel: CancellationToken,
) -> Option<tokio::task::JoinHandle<()>> {
    const CANCEL: &str = "com.apple.itunes-client.syncCancelRequest";

    let mut notifications = NotificationProxyClient::connect(provider)
        .await
        .inspect_err(
            |error| tracing::warn!(%error, "mb2: proceeding without device cancel observer"),
        )
        .ok()?;
    notifications
        .observe_notification(CANCEL)
        .await
        .inspect_err(
            |error| tracing::warn!(%error, "mb2: proceeding without device cancel observer"),
        )
        .ok()?;

    let span = tracing::Span::current();
    Some(tokio::spawn(
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

    use super::verdict;
    use crate::engine_error::{EngineFailure, ErrorKind};

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
        let cases = [
            (207, ErrorKind::InvalidBackupPassword),
            (208, ErrorKind::DeviceLocked),
            (211, ErrorKind::FindMyEnabled),
        ];
        for (code, expected) in cases {
            let error = verdict(device_verdict(code), "restore failed").unwrap_err();
            assert_eq!(EngineFailure::from(error).kind, expected);
        }
    }
}
