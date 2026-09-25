//! The device console: one owned os_trace relay stream pulled record-by-record
//! through open / next / close.

use std::ffi::c_char;
use std::sync::Mutex as StdMutex;

use idevice::services::os_trace_relay::{LogLevel, OsTraceRelayClient, OsTraceRelayReceiver};
use idevice::IdeviceService;
use tokio_util::sync::CancellationToken;

use crate::engine_error::ErrorKind;
use crate::ffi::{
    engine_udid, guard_error, AvEngine, AvError, AV_STREAM_CLOSED, AV_STREAM_CONTINUE,
};
use crate::{block, guard, out_str, provider_for, to_json, write_err};

pub struct AvConsoleStream {
    receiver: StdMutex<Option<OsTraceRelayReceiver>>,
    cancel: CancellationToken,
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

/// Opens a structured console stream; the session handle lands in out_stream.
/// Records flow immediately — pull them with av_console_next.
#[no_mangle]
pub extern "C" fn av_console_open(
    engine: *mut AvEngine,
    udid: *const c_char,
    out_stream: *mut *mut AvConsoleStream,
    error: *mut AvError,
) -> i32 {
    guard_error(error, |err| {
        let Some((engine, udid)) = (unsafe { engine_udid(engine, udid, err) }) else {
            return ErrorKind::InvalidArgument.code();
        };
        let context = engine.context();
        if out_stream.is_null() {
            out_str(err, "bad console stream output");
            return ErrorKind::InvalidArgument.code();
        }
        unsafe { *out_stream = std::ptr::null_mut() };
        block(async {
            let fut = async {
                let provider = provider_for(context, &udid).await?;
                let client = OsTraceRelayClient::connect(&provider).await?;
                client.start_trace(None).await
            };
            match tokio::time::timeout(crate::timeouts::UI_CALL, fut).await {
                Ok(Ok(receiver)) => {
                    unsafe {
                        *out_stream = Box::into_raw(Box::new(AvConsoleStream {
                            receiver: StdMutex::new(Some(receiver)),
                            cancel: CancellationToken::new(),
                        }));
                    }
                    0
                }
                Ok(Err(e)) => write_err(err, &e),
                Err(_) => {
                    out_str(err, "console connect timed out");
                    ErrorKind::Internal.code()
                }
            }
        })
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
        let Some(stream) = (unsafe { stream.as_ref() }) else {
            return AV_STREAM_CLOSED;
        };
        let Some(mut rx) = crate::lock(&stream.receiver).take() else {
            return AV_STREAM_CLOSED;
        };
        let res = block(async {
            tokio::select! {
                biased;
                _ = stream.cancel.cancelled() => None,
                result = tokio::time::timeout(crate::timeouts::STREAM_TICK, rx.next()) => Some(result),
            }
        });
        match res {
            None => AV_STREAM_CLOSED,
            Some(Err(_)) => {
                *crate::lock(&stream.receiver) = Some(rx);
                AV_STREAM_CONTINUE // quiet window — cancel-check point for the caller
            }
            Some(Ok(Ok(log))) => {
                *crate::lock(&stream.receiver) = Some(rx);
                let (subsystem, category) = log
                    .label
                    .as_ref()
                    .map(|l| (l.subsystem.as_str(), l.category.as_str()))
                    .unwrap_or(("", ""));
                out_str(
                    out_json,
                    &to_json(&ConsoleRecord {
                        ts: log.timestamp.format("%H:%M:%S%.3f").to_string(),
                        level: level_str(&log.level),
                        pid: log.pid,
                        image: &log.image_name,
                        message: &log.message,
                        subsystem,
                        category,
                    }),
                );
                0
            }
            Some(Ok(Err(e))) => write_err(err, &e), // receiver dropped: session dead
        }
    })
}

#[no_mangle]
pub unsafe extern "C" fn av_console_cancel(stream: *mut AvConsoleStream) {
    if let Some(stream) = unsafe { stream.as_ref() } {
        stream.cancel.cancel();
    }
}

/// Releases a console session after its final `next` call has returned.
#[no_mangle]
pub unsafe extern "C" fn av_console_close(stream: *mut AvConsoleStream) {
    if !stream.is_null() {
        drop(unsafe { Box::from_raw(stream) });
    }
}
