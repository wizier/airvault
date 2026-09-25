//! Standalone SpringBoard lock-state observer on its own notification_proxy
//! connection. Go owns the lifecycle and runs it only while the device is active.

use std::ffi::c_char;
use std::sync::Arc;

use idevice::services::notification_proxy::NotificationProxyClient;
use idevice::IdeviceService;

use crate::bounded;
use crate::engine_error::EngineFailure;
use crate::ffi::{
    engine_udid, guard, guard_error, reset_out, write_failure, AvEngine, AvError, AV_STREAM_CLOSED,
};
use crate::provider::{provider_for, EngineContext};
use crate::pull_stream::{ItemSender, PullStream};
use crate::timeouts;

/// av_lock_observer_next events: the lock state changed / the device locked.
pub const AV_LOCK_EVENT_CHANGED: i32 = 1;
pub const AV_LOCK_EVENT_COMPLETE: i32 = 2;

pub struct AvLockStream {
    stream: PullStream<i32>,
}

async fn observe_lock_state(
    context: Arc<EngineContext>,
    udid: String,
    sender: ItemSender<i32>,
) -> Result<(), EngineFailure> {
    const LOCK_CHANGED: &str = "com.apple.springboard.lockstate";
    const LOCKED: &str = "com.apple.springboard.lockcomplete";

    let setup = async {
        let provider = provider_for(&context, &udid).await?;
        let mut notifications = NotificationProxyClient::connect(&provider).await?;
        notifications
            .observe_notifications(&[LOCK_CHANGED, LOCKED])
            .await?;
        Ok(notifications)
    };
    let mut notifications = bounded::within(timeouts::CONNECT, setup)
        .await
        .map_err(|error| EngineFailure::from_request("lock observer setup failed", error))?;

    loop {
        let name = notifications
            .receive_notification()
            .await
            .map_err(|error| EngineFailure::from_request("lock observer stream ended", error))?;
        let event = match name.as_str() {
            LOCK_CHANGED => AV_LOCK_EVENT_CHANGED,
            LOCKED => AV_LOCK_EVENT_COMPLETE,
            _ => continue,
        };
        if sender.send(Ok(event)).await.is_err() {
            return Ok(());
        }
    }
}

#[no_mangle]
pub extern "C" fn av_lock_observer_open(
    engine: *mut AvEngine,
    udid: *const c_char,
    out: *mut *mut AvLockStream,
    error: *mut AvError,
) -> i32 {
    guard_error(error, || {
        let (engine, udid) = unsafe { engine_udid(engine, udid) }?;
        let context = engine.context_arc();
        reset_out(out, "missing lock observer output")?;
        let stream = PullStream::spawn(|sender| observe_lock_state(context, udid, sender));
        unsafe { *out = Box::into_raw(Box::new(AvLockStream { stream })) };
        Ok(())
    })
}

#[no_mangle]
pub extern "C" fn av_lock_observer_next(
    stream: *mut AvLockStream,
    out_event: *mut i32,
    err: *mut *mut c_char,
) -> i32 {
    guard(err, || {
        let Some(observer) = (unsafe { stream.as_ref() }) else {
            return AV_STREAM_CLOSED;
        };
        if let Err(failure) = reset_out(out_event, "missing lock event output") {
            return write_failure(err, failure);
        }
        observer
            .stream
            .next(err, |event| unsafe { *out_event = event })
    })
}

#[no_mangle]
pub unsafe extern "C" fn av_lock_observer_close(stream: *mut AvLockStream) {
    if !stream.is_null() {
        unsafe { Box::from_raw(stream) }.stream.close();
    }
}

#[no_mangle]
pub unsafe extern "C" fn av_lock_observer_cancel(stream: *mut AvLockStream) {
    if let Some(observer) = unsafe { stream.as_ref() } {
        observer.stream.cancel();
    }
}
