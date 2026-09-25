//! Keeps the phone awake during transfers: the wireless-sync power assertion
//! (`com.apple.mobile.assertion_agent`) Finder's Wi-Fi sync holds. iOS releases
//! it when the socket closes; the timeout only backstops half-open TCP.

use std::time::Duration;

use idevice::{Idevice, IdeviceError};
use plist::Value;

use crate::AirvaultProvider;

const ASSERTION_SERVICE: &str = "com.apple.mobile.assertion_agent";
const WIRELESS_SYNC_TYPE: &str = "AMDPowerAssertionTypeWirelessSync";
// Apple's cadence (disassembled from AMPDeviceDiscoveryAgent): create with the
// 1200 s maximum AMDevicePowerAssertionCreate accepts, re-create over a fresh
// connection every 600 s, releasing the previous assertion after.
const ASSERTION_BACKSTOP_SECS: f64 = 1200.0;
const ASSERTION_RENEW: Duration = crate::timeouts::ASSERTION_RENEW;
const ASSERTION_RETRY: Duration = crate::timeouts::ASSERTION_RETRY;
const MAX_REPLY_BYTES: usize = 64 * 1024;

/// The device holds the assertion while this connection stays open; dropping
/// the value closes the socket, which releases it immediately.
struct PowerAssertion {
    _connection: Idevice,
}

/// Starts the assertion agent and creates one wireless-sync assertion.
async fn hold_wireless_sync(
    provider: &AirvaultProvider,
    name: &str,
) -> Result<PowerAssertion, IdeviceError> {
    let mut connection = crate::connect_service(provider, ASSERTION_SERVICE).await?;
    let mut request = plist::Dictionary::new();
    request.insert(
        "CommandKey".into(),
        Value::String("CommandCreateAssertion".into()),
    );
    request.insert(
        "AssertionTypeKey".into(),
        Value::String(WIRELESS_SYNC_TYPE.into()),
    );
    request.insert("AssertionNameKey".into(), Value::String(name.into()));
    request.insert(
        "AssertionTimeoutKey".into(),
        Value::Real(ASSERTION_BACKSTOP_SECS),
    );
    // Apple sends this as a binary plist (AMDServiceConnectionSendMessage, format 200).
    let mut body = Vec::new();
    plist::to_writer_binary(&mut body, &request)
        .map_err(|e| IdeviceError::UnexpectedResponse(format!("encode assertion request: {e}")))?;
    let mut framed = Vec::with_capacity(4 + body.len());
    framed.extend_from_slice(&(body.len() as u32).to_be_bytes());
    framed.extend_from_slice(&body);
    connection.send_raw(&framed).await?;
    let header = connection.read_raw(4).await?;
    let len = u32::from_be_bytes([header[0], header[1], header[2], header[3]]) as usize;
    if len == 0 || len > MAX_REPLY_BYTES {
        return Err(IdeviceError::UnexpectedResponse(format!(
            "assertion agent framed an implausible {len}-byte reply"
        )));
    }
    let reply = connection.read_raw(len).await?;
    let reply: Value = plist::from_bytes(&reply)
        .map_err(|e| IdeviceError::UnexpectedResponse(format!("decode assertion reply: {e}")))?;
    tracing::debug!(
        ?reply,
        "assertion agent acknowledged CommandCreateAssertion"
    );
    Ok(PowerAssertion {
        _connection: connection,
    })
}

/// Holds an assertion for the life of this future, renewing on Finder's
/// cadence; each new assertion replaces (and so releases) the previous one.
/// Best-effort and never completes — run it as a select! arm beside the transfer.
pub(crate) async fn keep_device_awake(provider: &AirvaultProvider, udid: &str, label: &str) {
    let name = format!("AirVault {label}");
    let mut held: Option<PowerAssertion> = None;
    let mut warned = false;
    loop {
        match hold_wireless_sync(provider, &name).await {
            Ok(assertion) => {
                if held.is_none() {
                    tracing::info!(udid = %udid, "wireless-sync power assertion held");
                }
                held = Some(assertion);
                warned = false;
                tokio::time::sleep(ASSERTION_RENEW).await;
            }
            Err(error) => {
                if warned {
                    tracing::debug!(udid = %udid, error = ?error, "power assertion retry failed");
                } else {
                    tracing::warn!(udid = %udid, error = ?error, "power assertion unavailable; an off-charger phone may sleep mid-transfer");
                    warned = true;
                }
                tokio::time::sleep(ASSERTION_RETRY).await;
            }
        }
    }
}
