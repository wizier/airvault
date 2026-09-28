//! Session-mode device activation: the mobileactivationd side of the flow.
//! HTTP to Apple's activation servers lives in Go; this module only talks to
//! the phone. The two blob-carrying commands are hand-rolled — the idevice
//! crate exposes only the parameterless ones. TODO(upstream): contribute them.

use std::ffi::c_char;
use std::future::Future;

use idevice::services::mobileactivationd::MobileActivationdClient;
use idevice::IdeviceError;
use plist::Value;
use tracing::Instrument;

use crate::engine_error::EngineFailure;
use crate::ffi::{block_bounded, engine_udid, guard_error, opt_owned, out_str, AvEngine, AvError};
use crate::logging::operation_span;
use crate::provider::{
    authed_lockdown, connect_service, plist_exchange, provider_for, AirvaultProvider, PlistFormat,
};
use crate::timeouts;

const ACTIVATION_SERVICE: &str = "com.apple.mobileactivationd";
// Activation payloads carry certificate chains; cap generously.
const MAX_REPLY_BYTES: usize = 4 * 1024 * 1024;

/// One framed command: length-prefixed XML plist out, length-prefixed plist
/// back — the crate's wire shape for this daemon. A reply carrying `Error`
/// is the device refusing the step.
async fn activation_command(
    provider: &AirvaultProvider,
    command: &str,
    extra: plist::Dictionary,
) -> Result<plist::Dictionary, IdeviceError> {
    let mut request: plist::Dictionary = [("Command", command)].into_iter().collect();
    request.extend(extra);
    // Fresh service connection per request — the daemon requires it (the crate
    // module and both canonical clients reconnect for every command).
    let mut connection = connect_service(provider, ACTIVATION_SERVICE).await?;
    let reply = plist_exchange(
        &mut connection,
        &request,
        PlistFormat::Xml,
        MAX_REPLY_BYTES,
        command,
    )
    .await?;
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
    let extra = [("Value", Value::Data(handshake))].into_iter().collect();
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
    let mut extra: plist::Dictionary = [("Value", Value::Data(record))].into_iter().collect();
    if !headers.is_empty() {
        let dict: plist::Dictionary = headers
            .into_iter()
            .filter_map(|(key, value)| match value {
                serde_json::Value::String(text) => Some((key, text)),
                _ => None,
            })
            .collect();
        extra.insert("ActivationResponseHeaders".into(), dict.into());
    }
    activation_command(provider, "HandleActivationInfoWithSessionRequest", extra).await?;
    let ack = async {
        let mut lc = authed_lockdown(provider).await?;
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

/// The preamble every activation export shares: one step against a freshly
/// looked-up provider, bounded and traced under the caller's job.
unsafe fn run_step<T, Fut>(
    engine: *mut AvEngine,
    udid: *const c_char,
    job_id: *const c_char,
    timeout_msg: &str,
    step: impl FnOnce(AirvaultProvider) -> Fut,
) -> Result<T, EngineFailure>
where
    Fut: Future<Output = Result<T, IdeviceError>>,
{
    let (engine, udid) = unsafe { engine_udid(engine, udid) }?;
    let span = operation_span(&unsafe { opt_owned(job_id) }, "activation", &udid);
    let context = engine.context();
    block_bounded(
        timeouts::DEVICE_WORK,
        timeout_msg,
        async move { step(provider_for(context, &udid).await?).await }.instrument(span),
    )
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
    guard_error(error, || {
        let timeout = "activation state read timed out";
        let state = unsafe {
            run_step(engine, udid, job_id, timeout, |provider| async move {
                MobileActivationdClient::new(&provider).state().await
            })
        }?;
        out_str(out_state, &state);
        Ok(())
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
    guard_error(error, || {
        let timeout = "activation session info timed out";
        let xml = unsafe {
            run_step(engine, udid, job_id, timeout, |provider| async move {
                session_info_xml(&provider).await
            })
        }?;
        out_str(out_xml, &xml);
        Ok(())
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
    guard_error(error, || {
        if handshake.is_null() || handshake_len == 0 {
            return Err(EngineFailure::invalid_argument("bad handshake response"));
        }
        let handshake = unsafe { std::slice::from_raw_parts(handshake, handshake_len) }.to_vec();
        let timeout = "activation info timed out";
        let xml = unsafe {
            run_step(engine, udid, job_id, timeout, |provider| async move {
                activation_info_xml(&provider, handshake).await
            })
        }?;
        out_str(out_xml, &xml);
        Ok(())
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
    guard_error(error, || {
        if record.is_null() || record_len == 0 {
            return Err(EngineFailure::invalid_argument("bad activation record"));
        }
        let record = unsafe { std::slice::from_raw_parts(record, record_len) }.to_vec();
        let headers = serde_json::from_str(&unsafe { opt_owned(headers_json) })
            .map_err(|_| EngineFailure::invalid_argument("bad activation response headers"))?;
        let timeout = "activation record apply timed out";
        unsafe {
            run_step(engine, udid, job_id, timeout, |provider| async move {
                apply_record(&provider, record, headers).await
            })
        }
    })
}
