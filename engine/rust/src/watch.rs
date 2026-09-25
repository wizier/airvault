//! Pull-based live presence over one cancellable usbmuxd Listen session.

use std::ffi::c_char;
use std::sync::Arc;

use idevice::usbmuxd::RawPacket;
use idevice::{IdeviceError, ReadWrite};
use tokio::io::{AsyncReadExt, AsyncWriteExt};

use crate::bounded;
use crate::discover::{snapshot, SnapshotItem};
use crate::engine_error::EngineFailure;
use crate::ffi::{
    engine_ref, guard, guard_error, out_str, reset_out, to_json, AvEngine, AvError,
    AV_STREAM_CLOSED,
};
use crate::provider::EngineContext;
use crate::pull_stream::{ItemSender, PullStream};
use crate::timeouts;

pub struct AvPresenceWatch {
    stream: PullStream<String>,
}

/// One complete muxer state: whether it is reachable, and what it lists.
#[derive(serde::Serialize)]
struct PresenceState {
    up: bool,
    devices: Vec<SnapshotItem>,
}

/// Opens one independently-owned presence watcher.
#[no_mangle]
pub extern "C" fn av_device_watch_open(
    engine: *mut AvEngine,
    out: *mut *mut AvPresenceWatch,
    error: *mut AvError,
) -> i32 {
    guard_error(error, || {
        let context = unsafe { engine_ref(engine) }?.context_arc();
        reset_out(out, "missing presence watcher output")?;
        let stream = PullStream::spawn(|sender| watch_loop(context, sender));
        unsafe { *out = Box::into_raw(Box::new(AvPresenceWatch { stream })) };
        Ok(())
    })
}

/// Pulls the next complete state JSON via the AV_STREAM_* pull protocol.
#[no_mangle]
pub extern "C" fn av_device_watch_next(
    watcher: *mut AvPresenceWatch,
    out_json: *mut *mut c_char,
    err: *mut *mut c_char,
) -> i32 {
    guard(err, || {
        let Some(watcher) = (unsafe { watcher.as_ref() }) else {
            return AV_STREAM_CLOSED;
        };
        watcher.stream.next(err, |state| out_str(out_json, &state))
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
    // Starts true so a muxer that is down from the start is published too.
    let mut up = true;
    loop {
        let Err(error) = watch_once(&context, &sender, &mut up).await else {
            return Ok(());
        };
        tracing::debug!(%error, "presence watcher reconnecting");
        if up {
            up = false;
            let down = PresenceState {
                up: false,
                devices: Vec::new(),
            };
            if sender.send(Ok(to_json(&down))).await.is_err() {
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
) -> Result<(), IdeviceError> {
    let mut sock = bounded::within(timeouts::MUX, context.mux_addr().to_socket()).await?;

    let mut request = plist::Dictionary::new();
    request.insert("MessageType".into(), "Listen".into());
    request.insert("ClientVersionString".into(), "AirVault".into());
    request.insert("kLibUSBMuxVersion".into(), 3.into());
    let packet: Vec<u8> = RawPacket::new(request, 1, 8, 1).into();
    bounded::within(timeouts::MUX, async {
        sock.write_all(&packet).await?;
        Ok(())
    })
    .await?;

    let ack = bounded::within(timeouts::MUX, read_mux_message(&mut sock)).await?;
    if ack
        .get("Number")
        .and_then(|value| value.as_unsigned_integer())
        != Some(0)
    {
        return Err(IdeviceError::UnexpectedResponse(
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
) -> Result<plist::Dictionary, IdeviceError> {
    const MUX_HEADER: usize = 16;
    let mut header = [0u8; MUX_HEADER];
    socket.read_exact(&mut header).await?;
    let size = u32::from_le_bytes([header[0], header[1], header[2], header[3]]) as usize;
    if !(MUX_HEADER..=16 * 1024 * 1024).contains(&size) {
        return Err(IdeviceError::UnexpectedResponse(
            "bad muxer packet size".into(),
        ));
    }
    let mut body = vec![0u8; size - MUX_HEADER];
    socket.read_exact(&mut body).await?;
    Ok(plist::from_bytes(&body)?)
}

async fn emit_snapshot(
    context: &EngineContext,
    sender: &ItemSender<String>,
) -> Result<bool, IdeviceError> {
    let state = PresenceState {
        up: true,
        devices: snapshot(context).await?,
    };
    Ok(sender.send(Ok(to_json(&state))).await.is_ok())
}
