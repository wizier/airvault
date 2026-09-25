//! Finder-compatible iTunes sync session around a transfer: sync notifications
//! plus an exclusive AFC lock keep the device awake and serving keychain reads.

use std::future::Future;
use std::time::Duration;

use idevice::services::afc::{
    errors::AfcError,
    opcode::{AfcFopenMode, AfcLockOp},
    AfcClient,
};
use idevice::services::notification_proxy::NotificationProxyClient;
use idevice::IdeviceService;

use crate::afc::FileGuard;
use crate::bounded;
use crate::engine_error::EngineFailure;
use crate::provider::AirvaultProvider;
use crate::timeouts;

const LOCK_SYNC: &str = "/com.apple.itunes.lock_sync";
const SYNC_WILL_START: &str = "com.apple.itunes-mobdev.syncWillStart";
const SYNC_LOCK_REQUEST: &str = "com.apple.itunes-mobdev.syncLockRequest";
const SYNC_DID_START: &str = "com.apple.itunes-mobdev.syncDidStart";
const SYNC_DID_FINISH: &str = "com.apple.itunes-mobdev.syncDidFinish";
/// Another host (Finder, iTunes) may hold the sync lock; wait briefly, then
/// report the conflict rather than queueing behind it.
const SYNC_LOCK_WAIT: Duration = Duration::from_secs(10);
/// Retry cadence while polling for that lock.
const SYNC_LOCK_RETRY: Duration = Duration::from_millis(200);

pub(super) struct SyncSession {
    notifications: NotificationProxyClient,
    file: FileGuard,
}

impl SyncSession {
    /// Required: without it a locked or unpowered phone may sleep mid-transfer
    /// and deny protected keychain reads.
    pub(super) async fn open(
        provider: &AirvaultProvider,
        udid: &str,
    ) -> Result<Self, EngineFailure> {
        Self::start(provider).await.map_err(|error| {
            EngineFailure::from_idevice(
                &format!("could not establish the required sync session for {udid}"),
                error,
            )
        })
    }

    /// syncWillStart → open and lock the sync file → syncDidStart.
    async fn start(provider: &AirvaultProvider) -> Result<Self, idevice::IdeviceError> {
        let mut notifications =
            bounded::within(timeouts::PROBE, NotificationProxyClient::connect(provider)).await?;
        let file = match acquire(provider, &mut notifications).await {
            Ok(file) => file,
            Err(error) => {
                if let Err(cleanup_error) = cleanup_step(
                    "syncDidFinish notification after start failure",
                    notifications.post_notification(SYNC_DID_FINISH),
                )
                .await
                {
                    tracing::warn!(
                        error = %cleanup_error,
                        "sync session start failed and notification cleanup was incomplete"
                    );
                }
                return Err(error);
            }
        };

        let mut session = Self {
            notifications,
            file,
        };
        let did_start = session.notifications.post_notification(SYNC_DID_START);
        if let Err(error) = bounded::within(timeouts::PROBE, did_start).await {
            if let Err(cleanup_error) = session.finish().await {
                tracing::warn!(
                    error = %cleanup_error,
                    "sync session start failed and cleanup was incomplete"
                );
            }
            return Err(error);
        }
        Ok(session)
    }

    /// Unlock + close + syncDidFinish. Each step is independently bounded so
    /// one stuck socket cannot prevent later cleanup attempts.
    pub(super) async fn finish(self) -> Result<(), String> {
        let Self {
            mut notifications,
            file,
        } = self;
        let mut errors = Vec::new();
        if let Err(error) = cleanup_sync_file(file).await {
            errors.push(error);
        }
        if let Err(error) = cleanup_step(
            "syncDidFinish notification",
            notifications.post_notification(SYNC_DID_FINISH),
        )
        .await
        {
            errors.push(error);
        }
        if errors.is_empty() {
            Ok(())
        } else {
            Err(format!(
                "sync session cleanup was incomplete: {}",
                errors.join("; ")
            ))
        }
    }
}

async fn cleanup_step<T>(
    stage: &str,
    future: impl Future<Output = Result<T, idevice::IdeviceError>>,
) -> Result<(), String> {
    bounded::step(timeouts::TEARDOWN, stage, future).await
}

async fn cleanup_sync_file(mut file: FileGuard) -> Result<(), String> {
    let mut errors = Vec::new();
    if let Err(error) = cleanup_step("sync unlock", file.lock(AfcLockOp::Unlock)).await {
        errors.push(error);
    }
    if let Err(error) = cleanup_step("sync file close", file.close()).await {
        errors.push(error);
    }
    if errors.is_empty() {
        Ok(())
    } else {
        Err(errors.join("; "))
    }
}

/// syncWillStart → open the sync file → request and take its lock.
async fn acquire(
    provider: &AirvaultProvider,
    notifications: &mut NotificationProxyClient,
) -> Result<FileGuard, idevice::IdeviceError> {
    let will_start = notifications.post_notification(SYNC_WILL_START);
    bounded::within(timeouts::PROBE, will_start).await?;
    let afc = bounded::within(timeouts::PROBE, AfcClient::connect(provider)).await?;
    let file =
        bounded::within(timeouts::PROBE, afc.open_owned(LOCK_SYNC, AfcFopenMode::Rw)).await?;
    let mut file = FileGuard::new(file);
    let lock_request = notifications.post_notification(SYNC_LOCK_REQUEST);
    if let Err(error) = bounded::within(timeouts::PROBE, lock_request).await {
        return Err(preserve_acquire_error(file, error).await);
    }

    let deadline = tokio::time::Instant::now() + SYNC_LOCK_WAIT;
    loop {
        if tokio::time::Instant::now() >= deadline {
            return Err(preserve_acquire_error(
                file,
                idevice::IdeviceError::Afc(AfcError::ObjectBusy),
            )
            .await);
        }
        match tokio::time::timeout_at(deadline, file.lock(AfcLockOp::ExclusiveLock)).await {
            Ok(Ok(())) => return Ok(file),
            Ok(Err(idevice::IdeviceError::Afc(AfcError::OpWouldBlock))) => {
                tokio::time::sleep(SYNC_LOCK_RETRY).await;
            }
            Ok(Err(error)) => return Err(preserve_acquire_error(file, error).await),
            Err(_) => {
                return Err(preserve_acquire_error(file, idevice::IdeviceError::Timeout).await)
            }
        }
    }
}

async fn preserve_acquire_error(
    file: FileGuard,
    primary: idevice::IdeviceError,
) -> idevice::IdeviceError {
    if let Err(cleanup_error) = cleanup_sync_file(file).await {
        tracing::warn!(
            error = %cleanup_error,
            "sync lock acquisition failed and AFC cleanup was incomplete"
        );
    }
    primary
}
