//! Pull-based live presence over one cancellable usbmuxd Listen session.

use std::ffi::c_char;
use std::sync::Arc;

use idevice::usbmuxd::RawPacket;
use idevice::ReadWrite;
use tokio::io::{AsyncReadExt, AsyncWriteExt};

use crate::discover::snapshot_json;
use crate::engine_error::{EngineFailure, ErrorKind};
use crate::ffi::{engine_ref, guard_error, AvEngine, AvError, AV_STREAM_CLOSED};
use crate::pull_stream::{ItemSender, PullStream};
use crate::timeouts;
use crate::{guard, mux_bound, out_str, EngineContext};

pub struct AvPresenceWatch {
    stream: PullStream<String>,
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
        let stream = PullStream::spawn(|sender| watch_loop(context, sender));
        unsafe { *out = Box::into_raw(Box::new(AvPresenceWatch { stream })) };
        0
    })
}

/// Pulls the next complete versioned state JSON via the AV_STREAM_* pull protocol.
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
        match watcher.stream.next(err) {
            Ok(state) => {
                out_str(out_json, &state);
                0
            }
            Err(rc) => rc,
        }
    })
}

/// Signals cancellation without releasing the watcher.
#[no_mangle]
pub unsafe extern "C" fn av_device_watch_cancel(watcher: *mut AvPresenceWatch) {
    if let Some(watcher) = unsafe { watcher.as_ref() } {
        watcher.stream.cancel();
    }
}

/// Joins and releases a watcher after its final `next` call has returned.
#[no_mangle]
pub unsafe extern "C" fn av_device_watch_close(watcher: *mut AvPresenceWatch) {
    if !watcher.is_null() {
        unsafe { Box::from_raw(watcher) }.stream.close();
    }
}

/// Publishes a snapshot per muxer change, reconnecting until the reader goes
/// away. An unreachable muxer is published once as `up: false`.
async fn watch_loop(
    context: Arc<EngineContext>,
    sender: ItemSender<String>,
) -> Result<(), EngineFailure> {
    let mut up = false;
    let mut state_known = false;
    loop {
        let Err(error) = watch_once(&context, &sender, &mut up).await else {
            return Ok(());
        };
        tracing::debug!(%error, "presence watcher reconnecting");
        if !state_known || up {
            up = false;
            state_known = true;
            if sender.send(Ok(state_json(false, "[]"))).await.is_err() {
                return Ok(());
            }
        }
        tokio::time::sleep(timeouts::POLL_INTERVAL).await;
    }
}

/// Runs one Listen session; Ok means the reader is gone.
async fn watch_once(
    context: &EngineContext,
    sender: &ItemSender<String>,
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
        let message = read_mux_message(&mut sock).await?;
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
    sender: &ItemSender<String>,
) -> Result<bool, idevice::IdeviceError> {
    let devices = snapshot_json(context).await?;
    Ok(sender.send(Ok(state_json(true, &devices))).await.is_ok())
}
