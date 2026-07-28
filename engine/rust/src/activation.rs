//! Session-mode device activation: the mobileactivationd side of the flow.
//! HTTP to Apple's activation servers lives in Go; this module only talks to
//! the phone. The two blob-carrying commands are hand-rolled — the idevice
//! crate exposes only the parameterless ones. TODO(upstream): contribute them.

use std::ffi::c_char;
use std::time::Duration;

use idevice::services::mobileactivationd::MobileActivationdClient;
use idevice::{Idevice, IdeviceError};
use plist::Value;
use tracing::Instrument;

use crate::engine_error::ErrorKind;
use crate::ffi::{engine_udid, guard_error, AvEngine, AvError};
use crate::{
    block_bounded_out, block_bounded_unit, operation_span, opt_owned, out_str, provider_for,
    AirvaultProvider,
};

const ACTIVATION_SERVICE: &str = "com.apple.mobileactivationd";
const ACTIVATION_STEP_TIMEOUT: Duration = Duration::from_secs(30);
// Activation payloads carry certificate chains; cap generously.
const MAX_REPLY_BYTES: usize = 4 * 1024 * 1024;

/// Fresh service connection per request — the daemon requires it (the crate
/// module and both canonical clients reconnect for every command).
async fn activation_connect(provider: &AirvaultProvider) -> Result<Idevice, IdeviceError> {
    crate::connect_service(provider, ACTIVATION_SERVICE).await
}

/// One framed command: length-prefixed XML plist out, length-prefixed plist
/// back — the crate's wire shape for this daemon. A reply carrying `Error`
/// is the device refusing the step.
async fn activation_command(
    provider: &AirvaultProvider,
    command: &str,
    extra: plist::Dictionary,
) -> Result<plist::Dictionary, IdeviceError> {
    let mut request = plist::Dictionary::new();
    request.insert("Command".into(), Value::String(command.into()));
    for (key, value) in extra {
        request.insert(key, value);
    }
    let mut body = Vec::new();
    plist::to_writer_xml(&mut body, &request)
        .map_err(|e| IdeviceError::UnexpectedResponse(format!("encode {command}: {e}")))?;
    let mut connection = activation_connect(provider).await?;
    let mut framed = Vec::with_capacity(4 + body.len());
    framed.extend_from_slice(&(body.len() as u32).to_be_bytes());
    framed.extend_from_slice(&body);
    connection.send_raw(&framed).await?;
    let header = connection.read_raw(4).await?;
    let len = u32::from_be_bytes([header[0], header[1], header[2], header[3]]) as usize;
    if len == 0 || len > MAX_REPLY_BYTES {
        return Err(IdeviceError::UnexpectedResponse(format!(
            "mobileactivationd framed an implausible {len}-byte reply"
        )));
    }
    let reply = connection.read_raw(len).await?;
    let reply: Value = plist::from_bytes(&reply)
        .map_err(|e| IdeviceError::UnexpectedResponse(format!("decode {command} reply: {e}")))?;
    let Value::Dictionary(dict) = reply else {
        return Err(IdeviceError::UnexpectedResponse(format!(
            "{command} reply is not a dictionary"
        )));
    };
    if let Some(error) = dict.get("Error") {
        return Err(IdeviceError::UnexpectedResponse(format!(
            "{command} failed on the device: {error:?}"
        )));
    }
    Ok(dict)
}

fn value_to_xml(what: &str, value: &Value) -> Result<String, IdeviceError> {
    let mut out = Vec::new();
    plist::to_writer_xml(&mut out, value)
        .map_err(|e| IdeviceError::UnexpectedResponse(format!("encode {what}: {e}")))?;
    String::from_utf8(out)
        .map_err(|e| IdeviceError::UnexpectedResponse(format!("{what} is not UTF-8: {e}")))
}

async fn session_info_xml(provider: &AirvaultProvider) -> Result<String, IdeviceError> {
    let value = MobileActivationdClient::new(provider)
        .create_session_info()
        .await?;
    value_to_xml("session info", &value)
}

/// CreateTunnel1ActivationInfoRequest: Value = the raw drmHandshake response
/// bytes as `<data>`, like pymobiledevice3. Canon C hands over the parsed plist
/// and re-serializes it to the same XML.
async fn activation_info_xml(
    provider: &AirvaultProvider,
    handshake: Vec<u8>,
) -> Result<String, IdeviceError> {
    let mut extra = plist::Dictionary::new();
    extra.insert("Value".into(), Value::Data(handshake));
    let reply = activation_command(provider, "CreateTunnel1ActivationInfoRequest", extra).await?;
    let value = reply.get("Value").ok_or_else(|| {
        IdeviceError::UnexpectedResponse("activation info reply is missing Value".into())
    })?;
    value_to_xml("activation info", value)
}

/// HandleActivationInfoWithSessionRequest applies the record. The lockdown
/// acknowledgement mirrors the canonical clients but stays best-effort: the
/// phone is activated once the daemon accepts the record.
async fn apply_record(
    provider: &AirvaultProvider,
    record: Vec<u8>,
    headers: serde_json::Map<String, serde_json::Value>,
) -> Result<(), IdeviceError> {
    let mut extra = plist::Dictionary::new();
    extra.insert("Value".into(), Value::Data(record));
    if !headers.is_empty() {
        let mut dict = plist::Dictionary::new();
        for (key, value) in headers {
            if let serde_json::Value::String(text) = value {
                dict.insert(key, Value::String(text));
            }
        }
        extra.insert("ActivationResponseHeaders".into(), Value::Dictionary(dict));
    }
    activation_command(provider, "HandleActivationInfoWithSessionRequest", extra).await?;
    let ack = async {
        let mut lc = crate::authed_lockdown(provider).await?;
        lc.set_value("ActivationStateAcknowledged", true.into(), None)
            .await
    };
    if let Err(error) = ack.await {
        tracing::warn!(
            ?error,
            "activation succeeded but the acknowledgement write failed"
        );
    }
    Ok(())
}

/// Reads the live activation state ("Unactivated", "Activated", ...).
#[no_mangle]
pub extern "C" fn av_activation_state(
    engine: *mut AvEngine,
    udid: *const c_char,
    job_id: *const c_char,
    out_state: *mut *mut c_char,
    error: *mut AvError,
) -> i32 {
    guard_error(error, |err| {
        let Some((engine, udid)) = (unsafe { engine_udid(engine, udid, err) }) else {
            return ErrorKind::InvalidArgument.code();
        };
        let context = engine.context();
        let job_id = unsafe { opt_owned(job_id) };
        let span = operation_span(&job_id, "activation", &udid);
        block_bounded_out(
            out_state,
            err,
            ACTIVATION_STEP_TIMEOUT,
            "activation state read timed out",
            async move {
                let fut = async {
                    let provider = provider_for(context, &udid).await?;
                    MobileActivationdClient::new(&provider).state().await
                };
                fut.await.map_err(|e| format!("{e:?}"))
            }
            .instrument(span),
        )
    })
}

/// Session blob for the drmHandshake POST, as an XML plist string.
#[no_mangle]
pub extern "C" fn av_activation_session_info(
    engine: *mut AvEngine,
    udid: *const c_char,
    job_id: *const c_char,
    out_xml: *mut *mut c_char,
    error: *mut AvError,
) -> i32 {
    guard_error(error, |err| {
        let Some((engine, udid)) = (unsafe { engine_udid(engine, udid, err) }) else {
            return ErrorKind::InvalidArgument.code();
        };
        let context = engine.context();
        let job_id = unsafe { opt_owned(job_id) };
        let span = operation_span(&job_id, "activation", &udid);
        block_bounded_out(
            out_xml,
            err,
            ACTIVATION_STEP_TIMEOUT,
            "activation session info timed out",
            async move {
                let fut = async {
                    let provider = provider_for(context, &udid).await?;
                    session_info_xml(&provider).await
                };
                fut.await.map_err(|e| format!("{e:?}"))
            }
            .instrument(span),
        )
    })
}

/// Builds the signed activation info from Apple's drmHandshake response.
#[no_mangle]
pub extern "C" fn av_activation_info(
    engine: *mut AvEngine,
    udid: *const c_char,
    job_id: *const c_char,
    handshake: *const u8,
    handshake_len: usize,
    out_xml: *mut *mut c_char,
    error: *mut AvError,
) -> i32 {
    guard_error(error, |err| {
        let Some((engine, udid)) = (unsafe { engine_udid(engine, udid, err) }) else {
            return ErrorKind::InvalidArgument.code();
        };
        let context = engine.context();
        if handshake.is_null() || handshake_len == 0 {
            out_str(err, "bad handshake response");
            return ErrorKind::InvalidArgument.code();
        }
        let handshake = unsafe { std::slice::from_raw_parts(handshake, handshake_len) }.to_vec();
        let job_id = unsafe { opt_owned(job_id) };
        let span = operation_span(&job_id, "activation", &udid);
        block_bounded_out(
            out_xml,
            err,
            ACTIVATION_STEP_TIMEOUT,
            "activation info timed out",
            async move {
                let fut = async {
                    let provider = provider_for(context, &udid).await?;
                    activation_info_xml(&provider, handshake).await
                };
                fut.await.map_err(|e| format!("{e:?}"))
            }
            .instrument(span),
        )
    })
}

/// Applies Apple's activation record; `headers_json` carries the activation
/// response headers as a flat JSON object (may be empty).
#[no_mangle]
pub extern "C" fn av_activation_finish(
    engine: *mut AvEngine,
    udid: *const c_char,
    job_id: *const c_char,
    record: *const u8,
    record_len: usize,
    headers_json: *const c_char,
    error: *mut AvError,
) -> i32 {
    guard_error(error, |err| {
        let Some((engine, udid)) = (unsafe { engine_udid(engine, udid, err) }) else {
            return ErrorKind::InvalidArgument.code();
        };
        let context = engine.context();
        if record.is_null() || record_len == 0 {
            out_str(err, "bad activation record");
            return ErrorKind::InvalidArgument.code();
        }
        let record = unsafe { std::slice::from_raw_parts(record, record_len) }.to_vec();
        let headers_json = unsafe { opt_owned(headers_json) };
        let headers = if headers_json.is_empty() {
            serde_json::Map::new()
        } else {
            match serde_json::from_str(&headers_json) {
                Ok(headers) => headers,
                Err(_) => {
                    out_str(err, "bad activation response headers");
                    return ErrorKind::InvalidArgument.code();
                }
            }
        };
        let job_id = unsafe { opt_owned(job_id) };
        let span = operation_span(&job_id, "activation", &udid);
        block_bounded_unit(
            err,
            ACTIVATION_STEP_TIMEOUT,
            "activation record apply timed out",
            async move {
                let fut = async {
                    let provider = provider_for(context, &udid).await?;
                    apply_record(&provider, record, headers).await
                };
                fut.await.map_err(|e| format!("{e:?}"))
            }
            .instrument(span),
        )
    })
}
