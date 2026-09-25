//! Timeout and cancellation wrappers shared by device operations.

use std::future::Future;
use std::time::Duration;

use idevice::IdeviceError;
use tokio_util::sync::CancellationToken;

#[derive(Clone, Copy, Debug)]
pub(crate) enum Interrupt {
    Cancelled,
    TimedOut,
}

/// Await `future` under `limit`, flattening an elapsed limit into
/// `IdeviceError::Timeout`.
pub(crate) async fn within<T>(
    limit: Duration,
    future: impl Future<Output = Result<T, IdeviceError>>,
) -> Result<T, IdeviceError> {
    tokio::time::timeout(limit, future)
        .await
        .unwrap_or(Err(IdeviceError::Timeout))
}

/// Await a teardown step under `limit`, flattening its value to `()` and its
/// error or elapsed timeout to a `stage`-labelled String.
pub(crate) async fn step<T>(
    limit: Duration,
    stage: &str,
    future: impl Future<Output = Result<T, IdeviceError>>,
) -> Result<(), String> {
    match tokio::time::timeout(limit, future).await {
        Ok(Ok(_)) => Ok(()),
        Ok(Err(error)) => Err(format!("{stage} failed: {error} [{error:?}]")),
        Err(_) => Err(format!("{stage} timed out after {}s", limit.as_secs())),
    }
}

/// Await one stage without letting either a pending cancellation or a wedged
/// phone keep the operation stuck.
pub(crate) async fn cancel_or_timeout<T>(
    cancel: &CancellationToken,
    limit: Duration,
    future: impl Future<Output = T>,
) -> Result<T, Interrupt> {
    tokio::select! {
        biased;
        _ = cancel.cancelled() => Err(Interrupt::Cancelled),
        result = tokio::time::timeout(limit, future) => {
            result.map_err(|_| Interrupt::TimedOut)
        }
    }
}
