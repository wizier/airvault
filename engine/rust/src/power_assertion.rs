//! Keeps the phone awake during transfers: the wireless-sync power assertion
//! (`com.apple.mobile.assertion_agent`) Finder's Wi-Fi sync holds. iOS releases
//! it when the socket closes; the timeout only backstops half-open TCP.

use std::time::Duration;

use idevice::{Idevice, IdeviceError};
use plist::Value;

use crate::bounded;
use crate::provider::{connect_service, plist_exchange, AirvaultProvider, PlistFormat};
use crate::timeouts;

const ASSERTION_SERVICE: &str = "com.apple.mobile.assertion_agent";
const WIRELESS_SYNC_TYPE: &str = "AMDPowerAssertionTypeWirelessSync";
// Apple's cadence (disassembled from AMPDeviceDiscoveryAgent): create with the
// 1200 s maximum AMDevicePowerAssertionCreate accepts, re-create over a fresh
// connection every 600 s, releasing the previous assertion after.
const ASSERTION_BACKSTOP_SECS: f64 = 1200.0;
const ASSERTION_RENEW: Duration = Duration::from_secs(600);
/// Retry cadence when a renewal fails mid-transfer.
const ASSERTION_RETRY: Duration = Duration::from_secs(60);
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
    let mut connection = connect_service(provider, ASSERTION_SERVICE).await?;
    let request: plist::Dictionary = [
        ("CommandKey", Value::from("CommandCreateAssertion")),
        ("AssertionTypeKey", WIRELESS_SYNC_TYPE.into()),
        ("AssertionNameKey", name.into()),
        ("AssertionTimeoutKey", ASSERTION_BACKSTOP_SECS.into()),
    ]
    .into_iter()
    .collect();
    // Apple sends this as a binary plist (AMDServiceConnectionSendMessage, format 200).
    let reply = plist_exchange(
        &mut connection,
        &request,
        PlistFormat::Binary,
        MAX_REPLY_BYTES,
        "assertion",
    )
    .await?;
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
        // Bounded, or a half-open socket would stall every later renewal.
        match bounded::within(timeouts::CONNECT, hold_wireless_sync(provider, &name)).await {
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
