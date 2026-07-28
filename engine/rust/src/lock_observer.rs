//! Standalone SpringBoard lock-state observer on its own notification_proxy
//! connection. Go owns the lifecycle and runs it only while the device is active.

use std::ffi::c_char;
use std::sync::mpsc::{channel, Receiver, RecvTimeoutError, Sender};
use std::sync::Mutex;
use std::time::Duration;

use idevice::services::notification_proxy::NotificationProxyClient;
use idevice::IdeviceService;
use tokio_util::sync::CancellationToken;

use crate::bounded::{cancel_or_timeout, Interrupt};
use crate::engine_error::ErrorKind;
use crate::ffi::{
    engine_udid, guard_error, AvEngine, AvError, AV_STREAM_CLOSED, AV_STREAM_CONTINUE,
};
use crate::{guard, out_str, provider_for, EngineContext};

const SETUP_TIMEOUT: Duration = Duration::from_secs(10);

pub struct AvLockStream {
    receiver: Mutex<Receiver<Result<i32, String>>>,
    cancel: CancellationToken,
    task: Option<tokio::task::JoinHandle<()>>,
}

async fn run(
    context: &EngineContext,
    udid: &str,
    cancel: CancellationToken,
    sender: Sender<Result<i32, String>>,
) -> Result<(), String> {
    const LOCK_CHANGED: &str = "com.apple.springboard.lockstate";
    const LOCKED: &str = "com.apple.springboard.lockcomplete";

    let setup = async {
        let provider = provider_for(context, udid).await.map_err(|error| {
            format!("lock observer provider lookup failed: {error} [{error:?}]")
        })?;
        let mut notifications = NotificationProxyClient::connect(&provider)
            .await
            .map_err(|error| format!("lock observer connect failed: {error} [{error:?}]"))?;
        notifications
            .observe_notifications(&[LOCK_CHANGED, LOCKED])
            .await
            .map_err(|error| format!("lock observer subscribe failed: {error} [{error:?}]"))?;
        Ok::<_, String>(notifications)
    };

    let mut notifications = match cancel_or_timeout(&cancel, SETUP_TIMEOUT, setup).await {
        Ok(result) => result?,
        Err(Interrupt::Cancelled) => return Ok(()),
        Err(Interrupt::TimedOut) => {
            return Err(format!(
                "lock observer setup timed out after {}s",
                SETUP_TIMEOUT.as_secs()
            ))
        }
    };

    loop {
        let name = tokio::select! {
            biased;
            _ = cancel.cancelled() => return Ok(()),
            result = notifications.receive_notification() => {
                result.map_err(|error| {
                    format!("lock observer stream ended: {error} [{error:?}]")
                })?
            }
        };
        let event = match name.as_str() {
            LOCK_CHANGED => 1,
            LOCKED => 2,
            _ => continue,
        };
        if sender.send(Ok(event)).is_err() {
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
    guard_error(error, |err| {
        let Some((engine, udid)) = (unsafe { engine_udid(engine, udid, err) }) else {
            return ErrorKind::InvalidArgument.code();
        };
        let context = engine.context_arc();
        if out.is_null() {
            out_str(err, "missing lock observer output");
            return ErrorKind::InvalidArgument.code();
        }
        let (sender, receiver) = channel();
        let cancel = CancellationToken::new();
        let worker_cancel = cancel.clone();
        let terminal = sender.clone();
        let task = crate::spawn(async move {
            if let Err(message) = run(&context, &udid, worker_cancel, sender).await {
                let _ = terminal.send(Err(message));
            }
        });
        let stream = Box::new(AvLockStream {
            receiver: Mutex::new(receiver),
            cancel,
            task: Some(task),
        });
        unsafe { *out = Box::into_raw(stream) };
        0
    })
}

#[no_mangle]
pub extern "C" fn av_lock_observer_next(
    stream: *mut AvLockStream,
    out_event: *mut i32,
    err: *mut *mut c_char,
) -> i32 {
    guard(err, || {
        let Some(stream) = (unsafe { stream.as_ref() }) else {
            out_str(err, "lock observer is closed");
            return AV_STREAM_CLOSED;
        };
        if out_event.is_null() {
            out_str(err, "missing lock event output");
            return ErrorKind::InvalidArgument.code();
        }
        match crate::lock(&stream.receiver).recv_timeout(Duration::from_secs(1)) {
            Ok(Ok(event)) => {
                unsafe { *out_event = event };
                0
            }
            Ok(Err(message)) => {
                out_str(err, &message);
                ErrorKind::Internal.code()
            }
            Err(RecvTimeoutError::Timeout) => AV_STREAM_CONTINUE,
            Err(RecvTimeoutError::Disconnected) => AV_STREAM_CLOSED,
        }
    })
}

#[no_mangle]
pub unsafe extern "C" fn av_lock_observer_close(stream: *mut AvLockStream) {
    if stream.is_null() {
        return;
    }
    let mut stream = unsafe { Box::from_raw(stream) };
    stream.cancel.cancel();
    if let Some(task) = stream.task.take() {
        let _ = crate::block(task);
    }
}

#[no_mangle]
pub unsafe extern "C" fn av_lock_observer_cancel(stream: *mut AvLockStream) {
    if let Some(stream) = unsafe { stream.as_ref() } {
        stream.cancel.cancel();
    }
}
