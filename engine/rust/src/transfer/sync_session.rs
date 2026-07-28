//! Finder-compatible iTunes sync session around a transfer: sync notifications
//! plus an exclusive AFC lock keep the device awake and serving keychain reads.

use std::future::Future;
use std::time::Duration;

use idevice::services::afc::{
    errors::AfcError,
    file::OwnedFileDescriptor,
    opcode::{AfcFopenMode, AfcLockOp},
    AfcClient,
};
use idevice::services::notification_proxy::NotificationProxyClient;
use idevice::IdeviceService;

use crate::bounded;
use crate::engine_error::EngineFailure;
use crate::AirvaultProvider;

const LOCK_SYNC: &str = "/com.apple.itunes.lock_sync";
const START_STEP_TIMEOUT: Duration = Duration::from_secs(5);
const LOCK_WAIT_TIMEOUT: Duration = Duration::from_secs(10);
const CLEANUP_STEP_TIMEOUT: Duration = Duration::from_secs(2);

pub(super) struct SyncSession {
    notifications: NotificationProxyClient,
    file: OwnedFileDescriptor,
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
        let mut notifications = start_step(NotificationProxyClient::connect(provider)).await?;
        if let Err(error) =
            start_step(notifications.post_notification("com.apple.itunes-mobdev.syncWillStart"))
                .await
        {
            let _ = cleanup_step(
                "syncDidFinish notification after syncWillStart failure",
                notifications.post_notification("com.apple.itunes-mobdev.syncDidFinish"),
            )
            .await;
            return Err(error);
        }

        let file = match acquire(provider, &mut notifications).await {
            Ok(file) => file,
            Err(error) => {
                if let Err(cleanup_error) = cleanup_step(
                    "syncDidFinish notification after acquire failure",
                    notifications.post_notification("com.apple.itunes-mobdev.syncDidFinish"),
                )
                .await
                {
                    tracing::warn!(
                        error = %cleanup_error,
                        "sync lock acquisition failed and notification cleanup was incomplete"
                    );
                }
                return Err(error);
            }
        };

        let mut session = Self {
            notifications,
            file,
        };
        if let Err(error) = start_step(
            session
                .notifications
                .post_notification("com.apple.itunes-mobdev.syncDidStart"),
        )
        .await
        {
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
            notifications.post_notification("com.apple.itunes-mobdev.syncDidFinish"),
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

async fn start_step<T>(
    future: impl Future<Output = Result<T, idevice::IdeviceError>>,
) -> Result<T, idevice::IdeviceError> {
    tokio::time::timeout(START_STEP_TIMEOUT, future)
        .await
        .unwrap_or(Err(idevice::IdeviceError::Timeout))
}

async fn cleanup_step<T>(
    stage: &str,
    future: impl Future<Output = Result<T, idevice::IdeviceError>>,
) -> Result<(), String> {
    bounded::step(CLEANUP_STEP_TIMEOUT, stage, future).await
}

async fn cleanup_sync_file(mut file: OwnedFileDescriptor) -> Result<(), String> {
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

async fn acquire(
    provider: &AirvaultProvider,
    notifications: &mut NotificationProxyClient,
) -> Result<OwnedFileDescriptor, idevice::IdeviceError> {
    let afc = start_step(AfcClient::connect(provider)).await?;
    let mut file = start_step(afc.open_owned(LOCK_SYNC, AfcFopenMode::Rw)).await?;
    if let Err(error) =
        start_step(notifications.post_notification("com.apple.itunes-mobdev.syncLockRequest")).await
    {
        return Err(preserve_acquire_error(file, error).await);
    }

    let deadline = tokio::time::Instant::now() + LOCK_WAIT_TIMEOUT;
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
                tokio::time::sleep(Duration::from_millis(200)).await;
            }
            Ok(Err(error)) => return Err(preserve_acquire_error(file, error).await),
            Err(_) => {
                return Err(preserve_acquire_error(file, idevice::IdeviceError::Timeout).await)
            }
        }
    }
}

async fn preserve_acquire_error(
    file: OwnedFileDescriptor,
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
