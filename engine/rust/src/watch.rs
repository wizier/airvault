//! Pull-based live presence over one cancellable usbmuxd Listen session.

use std::ffi::c_char;
use std::sync::{Arc, Mutex as StdMutex};

use idevice::usbmuxd::RawPacket;
use idevice::ReadWrite;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::sync::watch as state_watch;
use tokio_util::sync::CancellationToken;

use crate::discover::snapshot_json;
use crate::engine_error::ErrorKind;
use crate::ffi::{
    engine_ref, guard_error, AvEngine, AvError, AV_STREAM_CLOSED, AV_STREAM_CONTINUE,
};
use crate::{guard, mux_bound, out_str, EngineContext};

pub struct AvPresenceWatch {
    receiver: StdMutex<Option<state_watch::Receiver<String>>>,
    cancel: CancellationToken,
    task: Option<tokio::task::JoinHandle<()>>,
}

/// Opens one independently-owned presence watcher.
#[no_mangle]
pub extern "C" fn av_device_watch_open(
    engine: *mut AvEngine,
    out: *mut *mut AvPresenceWatch,
    error: *mut AvError,
) -> i32 {
    guard_error(error, |err| {
        let Some(engine) = (unsafe { engine_ref(engine, err) }) else {
            return ErrorKind::InvalidArgument.code();
        };
        let context = engine.context_arc();
        if out.is_null() {
            out_str(err, "missing presence watcher output");
            return ErrorKind::InvalidArgument.code();
        }
        let (sender, receiver) = state_watch::channel(String::new());
        let cancel = CancellationToken::new();
        let task = crate::spawn(watch_loop(context, sender, cancel.clone()));
        let watcher = Box::new(AvPresenceWatch {
            receiver: StdMutex::new(Some(receiver)),
            cancel,
            task: Some(task),
        });
        unsafe { *out = Box::into_raw(watcher) };
        0
    })
}

/// Pulls the latest complete versioned state JSON via the AV_STREAM_* pull protocol.
#[no_mangle]
pub extern "C" fn av_device_watch_next(
    watcher: *mut AvPresenceWatch,
    out_json: *mut *mut c_char,
    err: *mut *mut c_char,
) -> i32 {
    guard(err, || {
        let Some(watcher) = (unsafe { watcher.as_ref() }) else {
            out_str(err, "presence watcher is closed");
            return AV_STREAM_CLOSED;
        };
        let Some(mut receiver) = crate::lock(&watcher.receiver).take() else {
            out_str(err, "presence watcher is closed");
            return AV_STREAM_CLOSED;
        };
        let result = crate::block(async {
            tokio::select! {
                biased;
                _ = watcher.cancel.cancelled() => None,
                result = tokio::time::timeout(crate::timeouts::STREAM_TICK, receiver.changed()) => Some(result),
            }
        });
        match result {
            None => AV_STREAM_CLOSED,
            Some(Err(_)) => {
                *crate::lock(&watcher.receiver) = Some(receiver);
                AV_STREAM_CONTINUE
            }
            Some(Ok(Ok(()))) => {
                let state = receiver.borrow_and_update().clone();
                *crate::lock(&watcher.receiver) = Some(receiver);
                out_str(out_json, &state);
                0
            }
            Some(Ok(Err(_))) => {
                out_str(err, "presence watcher stopped");
                AV_STREAM_CLOSED
            }
        }
    })
}

/// Signals cancellation without releasing the watcher.
#[no_mangle]
pub unsafe extern "C" fn av_device_watch_cancel(watcher: *mut AvPresenceWatch) {
    if let Some(watcher) = unsafe { watcher.as_ref() } {
        watcher.cancel.cancel();
    }
}

/// Joins and releases a watcher after its final `next` call has returned.
#[no_mangle]
pub unsafe extern "C" fn av_device_watch_close(watcher: *mut AvPresenceWatch) {
    if watcher.is_null() {
        return;
    }
    let mut watcher = unsafe { Box::from_raw(watcher) };
    watcher.cancel.cancel();
    if let Some(task) = watcher.task.take() {
        let _ = crate::block(task);
    }
}

async fn watch_loop(
    context: Arc<EngineContext>,
    sender: state_watch::Sender<String>,
    cancel: CancellationToken,
) {
    let mut up = false;
    let mut state_known = false;
    loop {
        let result = tokio::select! {
            _ = cancel.cancelled() => return,
            result = watch_once(&context, &sender, &cancel, &mut up) => result,
        };
        if let Err(error) = &result {
            tracing::debug!(%error, "presence watcher reconnecting");
        }
        if result.is_err() && (!state_known || up) {
            up = false;
            state_known = true;
            if sender.send(state_json(false, "[]")).is_err() {
                return;
            }
        }
        tokio::select! {
            _ = cancel.cancelled() => return,
            _ = tokio::time::sleep(crate::timeouts::POLL_INTERVAL) => {}
        }
    }
}

async fn watch_once(
    context: &EngineContext,
    sender: &state_watch::Sender<String>,
    cancel: &CancellationToken,
    up: &mut bool,
) -> Result<(), idevice::IdeviceError> {
    let mut sock = mux_bound(async { context.mux_addr().to_socket().await }).await?;

    let mut request = plist::Dictionary::new();
    request.insert("MessageType".into(), "Listen".into());
    request.insert("ClientVersionString".into(), "AirVault".into());
    request.insert("kLibUSBMuxVersion".into(), 3.into());
    let packet: Vec<u8> = RawPacket::new(request, 1, 8, 1).into();
    mux_bound(async {
        sock.write_all(&packet).await?;
        Ok(())
    })
    .await?;

    let ack = mux_bound(read_mux_message(&mut sock)).await?;
    if ack
        .get("Number")
        .and_then(|value| value.as_unsigned_integer())
        != Some(0)
    {
        return Err(idevice::IdeviceError::UnexpectedResponse(
            "usbmuxd Listen request refused".into(),
        ));
    }
    *up = true;
    if !emit_snapshot(context, sender).await? {
        return Ok(());
    }
    loop {
        let message = tokio::select! {
            _ = cancel.cancelled() => return Ok(()),
            result = read_mux_message(&mut sock) => result?,
        };
        let message_type = message
            .get("MessageType")
            .and_then(|value| value.as_string());
        if matches!(message_type, Some("Attached" | "Detached" | "Paired"))
            && !emit_snapshot(context, sender).await?
        {
            return Ok(());
        }
    }
}

async fn read_mux_message(
    socket: &mut Box<dyn ReadWrite>,
) -> Result<plist::Dictionary, idevice::IdeviceError> {
    const MUX_HEADER: usize = 16;
    let mut header = [0u8; MUX_HEADER];
    socket.read_exact(&mut header).await?;
    let size = u32::from_le_bytes([header[0], header[1], header[2], header[3]]) as usize;
    if !(MUX_HEADER..=16 * 1024 * 1024).contains(&size) {
        return Err(idevice::IdeviceError::UnexpectedResponse(
            "bad muxer packet size".into(),
        ));
    }
    let mut body = vec![0u8; size - MUX_HEADER];
    socket.read_exact(&mut body).await?;
    Ok(plist::from_bytes(&body)?)
}

fn state_json(up: bool, devices: &str) -> String {
    format!(r#"{{"version":1,"up":{up},"devices":{devices}}}"#)
}

async fn emit_snapshot(
    context: &EngineContext,
    sender: &state_watch::Sender<String>,
) -> Result<bool, idevice::IdeviceError> {
    let devices = snapshot_json(context).await?;
    Ok(sender.send(state_json(true, &devices)).is_ok())
}
