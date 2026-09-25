//! The device console: one owned os_trace relay stream pulled record-by-record
//! through open / next / close.

use std::ffi::c_char;

use idevice::services::os_trace_relay::{LogLevel, OsTraceRelayClient, OsTraceRelayReceiver};
use idevice::IdeviceService;

use crate::engine_error::EngineFailure;
use crate::ffi::{
    block_bounded, engine_udid, guard, guard_error, out_str, to_json, AvEngine, AvError,
    AV_STREAM_CLOSED,
};
use crate::provider::provider_for;
use crate::pull_stream::{ItemSender, PullStream};
use crate::timeouts;

pub struct AvConsoleStream {
    stream: PullStream<String>,
}

fn level_str(l: &LogLevel) -> &'static str {
    match l {
        LogLevel::Notice => "notice",
        LogLevel::Info => "info",
        LogLevel::Debug => "debug",
        LogLevel::Error => "error",
        LogLevel::Fault => "fault",
    }
}

#[derive(serde::Serialize)]
struct ConsoleRecord<'a> {
    ts: String,
    level: &'static str,
    pid: u32,
    image: &'a str,
    message: &'a str,
    subsystem: &'a str,
    category: &'a str,
}

/// Forwards every os_trace record as its JSON line until the reader goes away.
async fn forward_records(
    mut receiver: OsTraceRelayReceiver,
    sender: ItemSender<String>,
) -> Result<(), EngineFailure> {
    loop {
        let log = receiver.next().await?;
        let (subsystem, category) = log
            .label
            .as_ref()
            .map(|l| (l.subsystem.as_str(), l.category.as_str()))
            .unwrap_or(("", ""));
        let record = to_json(&ConsoleRecord {
            ts: log.timestamp.format("%H:%M:%S%.3f").to_string(),
            level: level_str(&log.level),
            pid: log.pid,
            image: &log.image_name,
            message: &log.message,
            subsystem,
            category,
        });
        if sender.send(Ok(record)).await.is_err() {
            return Ok(());
        }
    }
}

/// Opens a structured console stream; the session handle lands in out_stream.
/// Records flow immediately — pull them with av_console_next.
#[no_mangle]
pub extern "C" fn av_console_open(
    engine: *mut AvEngine,
    udid: *const c_char,
    out_stream: *mut *mut AvConsoleStream,
    error: *mut AvError,
) -> i32 {
    guard_error(error, || {
        let (engine, udid) = unsafe { engine_udid(engine, udid) }?;
        let context = engine.context();
        if out_stream.is_null() {
            return Err(EngineFailure::invalid_argument("bad console stream output"));
        }
        unsafe { *out_stream = std::ptr::null_mut() };
        let receiver = block_bounded(timeouts::UI_CALL, "console connect timed out", async {
            let provider = provider_for(context, &udid).await?;
            let client = OsTraceRelayClient::connect(&provider).await?;
            client.start_trace(None).await
        })?;
        let stream = PullStream::spawn(|sender| forward_records(receiver, sender));
        unsafe { *out_stream = Box::into_raw(Box::new(AvConsoleStream { stream })) };
        Ok(())
    })
}

/// Pulls the next log record as JSON via the AV_STREAM_* pull protocol:
/// 0 = record in out_json; any other rc is a stream error (session is dead).
#[no_mangle]
pub extern "C" fn av_console_next(
    stream: *mut AvConsoleStream,
    out_json: *mut *mut c_char,
    err: *mut *mut c_char,
) -> i32 {
    guard(err, || {
        let Some(console) = (unsafe { stream.as_ref() }) else {
            return AV_STREAM_CLOSED;
        };
        match console.stream.next(err) {
            Ok(record) => {
                out_str(out_json, &record);
                0
            }
            Err(rc) => rc,
        }
    })
}

#[no_mangle]
pub unsafe extern "C" fn av_console_cancel(stream: *mut AvConsoleStream) {
    if let Some(console) = unsafe { stream.as_ref() } {
        console.stream.cancel();
    }
}

/// Releases a console session after its final `next` call has returned.
#[no_mangle]
pub unsafe extern "C" fn av_console_close(stream: *mut AvConsoleStream) {
    if !stream.is_null() {
        unsafe { Box::from_raw(stream) }.stream.close();
    }
}
