//! Discovery & cheap device reads: enriched discover, the availability
//! snapshot shared with the watcher, battery and the USB list.

use std::ffi::c_char;
use std::time::Duration;

use idevice::provider::IdeviceProvider;
use idevice::services::lockdown::LockdownClient;
use idevice::usbmuxd::{Connection, UsbmuxdDevice};
use idevice::{IdeviceError, IdeviceService};
use plist::Value;
use tokio::time::{timeout, timeout_at, Instant};

use crate::engine_error::{EngineFailure, ErrorKind};
use crate::ffi::{
    block_bounded, engine_ref, engine_udid, guard_error, out_str, to_json, AvEngine, AvError,
};
use crate::provider::{
    authed_lockdown, block, devices_deduped, provider_for, provider_from, AirvaultProvider,
    EngineContext,
};
use crate::timeouts;

/// Discovery answers the device list: probe every phone and fail fast, because
/// one unreachable device must not stall the refresh worker.
const DISCOVERY_TIMEOUT: Duration = Duration::from_secs(4);

#[derive(serde::Serialize)]
pub(crate) struct SnapshotItem {
    udid: String,
    connection: &'static str,
}

/// The cheap availability snapshot: usbmuxd get_devices with NO lockdown
/// metadata, as `[{"udid","connection"}]`. Shared by the one-shot list and the
/// owned presence watcher so both speak the identical shape. An unclassified
/// connection is reported as usb (a locally attached device), never dropped.
pub(crate) async fn snapshot(context: &EngineContext) -> Result<Vec<SnapshotItem>, IdeviceError> {
    let devices = devices_deduped(context).await?;
    Ok(devices
        .into_iter()
        .map(|d| SnapshotItem {
            connection: match d.connection_type {
                Connection::Network(_) => "wifi",
                Connection::Usb | Connection::Unknown(_) => "usb",
            },
            udid: d.udid,
        })
        .collect())
}

/// The lockdown-enriched view of a device: identity plus pairing/sync/encryption
/// flags. Pairing is intentionally tri-state: a transport or lockdown failure
/// does not prove that a previously paired device became unpaired.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq, serde::Serialize)]
#[serde(rename_all = "lowercase")]
enum PairingState {
    Paired,
    Unpaired,
    #[default]
    Unknown,
}

#[derive(Default, serde::Serialize)]
#[serde(rename_all = "camelCase")]
struct DiscoverItem {
    udid: String,
    name: String,
    product_type: String,
    ios_version: String,
    pairing_state: PairingState,
    metadata_known: bool,
    flags_known: bool,
    encrypted: bool,
    activation_state: String,
}

/// Enrich a device over lockdown, time-bounded. Failure produces an explicit
/// unknown result instead of removing a mux-reachable device from the response.
async fn device_meta(context: &EngineContext, device: &UsbmuxdDevice) -> DiscoverItem {
    let mut item = DiscoverItem {
        udid: device.udid.clone(),
        name: device.udid.clone(),
        ..DiscoverItem::default()
    };
    let deadline = Instant::now() + DISCOVERY_TIMEOUT;
    let provider = provider_from(context, device);
    let Ok(Ok(mut lc)) = timeout_at(deadline, LockdownClient::connect(&provider)).await else {
        return item;
    };

    // Establish the pairing verdict first. Once a session succeeds, later
    // metadata or flag timeouts must not erase that confirmed result.
    item.pairing_state = pairing_state(&provider, &mut lc, deadline).await;

    // Identity is an all-or-nothing snapshot. Consumers must not overwrite
    // known metadata with empty strings when any GetValue read fails.
    if let Ok(Some((name, product_type, ios_version))) =
        timeout_at(deadline, read_identity(&mut lc)).await
    {
        item.name = name;
        item.product_type = product_type;
        item.ios_version = ios_version;
        item.metadata_known = true;
    }

    // A failed or malformed read is propagated to flagsKnown=false; a valid
    // boolean false remains distinguishable. The encryption flag does not
    // affect the already-confirmed pairing verdict.
    if item.pairing_state == PairingState::Paired {
        if let Ok(Ok(encrypted)) = timeout_at(deadline, read_will_encrypt(&mut lc)).await {
            item.encrypted = encrypted;
            item.flags_known = true;
        }
    }

    // Best-effort: an unactivated phone (Setup Assistant) cannot run mb2
    // operations, so the UI warns from this field. Empty = unknown.
    if let Ok(Ok(state)) =
        timeout_at(deadline, get_required_string(&mut lc, "ActivationState")).await
    {
        item.activation_state = state;
    }
    item
}

async fn pairing_state(
    provider: &AirvaultProvider,
    lc: &mut LockdownClient,
    deadline: Instant,
) -> PairingState {
    // A missing local pairing record surfaces as InvalidHostID from the
    // provider; the same variant from start_session means the phone revoked us.
    let pf = match provider.get_pairing_file().await {
        Ok(pf) => pf,
        Err(error) => return unpaired_or_unknown(&error),
    };

    match timeout_at(deadline, lc.start_session(&pf)).await {
        Ok(Ok(_)) => PairingState::Paired,
        Ok(Err(error)) => unpaired_or_unknown(&error),
        Err(_) => PairingState::Unknown,
    }
}

async fn read_identity(lc: &mut LockdownClient) -> Option<(String, String, String)> {
    let name = get_required_string(lc, "DeviceName").await.ok()?;
    let ptype = get_required_string(lc, "ProductType").await.ok()?;
    let ver = get_required_string(lc, "ProductVersion").await.ok()?;
    Some((name, ptype, ver))
}

async fn get_required_string(lc: &mut LockdownClient, key: &str) -> Result<String, IdeviceError> {
    lc.get_value(Some(key), None)
        .await?
        .as_string()
        .filter(|value| !value.is_empty())
        .map(str::to_owned)
        .ok_or_else(|| {
            IdeviceError::UnexpectedResponse(format!(
                "lockdown {key} is missing or not a non-empty string"
            ))
        })
}

fn unpaired_or_unknown(error: &IdeviceError) -> PairingState {
    match error {
        IdeviceError::InvalidHostID => PairingState::Unpaired,
        _ => PairingState::Unknown,
    }
}

/// The one lockdown read shared by discovery and the backup-password flow.
pub(crate) async fn read_will_encrypt(lc: &mut LockdownClient) -> Result<bool, IdeviceError> {
    lc.get_value(Some("WillEncrypt"), Some("com.apple.mobile.backup"))
        .await?
        .as_boolean()
        .ok_or_else(|| {
            IdeviceError::UnexpectedResponse("lockdown WillEncrypt is not a boolean".into())
        })
}

/// Discovers reachable devices WITH lockdown metadata (see DiscoverItem).
/// No connection field on purpose — presence/transport is the watcher's domain.
#[no_mangle]
pub extern "C" fn av_devices_inspect(
    engine: *mut AvEngine,
    out_json: *mut *mut c_char,
    error: *mut AvError,
) -> i32 {
    guard_error(error, || {
        let context = unsafe { engine_ref(engine) }?.context();
        let items = block(async {
            let mut items = Vec::new();
            for device in devices_deduped(context).await? {
                items.push(device_meta(context, &device).await);
            }
            Ok::<_, EngineFailure>(items)
        })?;
        out_str(out_json, &to_json(&items));
        Ok(())
    })
}

/// Cheap availability check (NO lockdown metadata): [{"udid","connection"}]
/// into out_json. A one-shot pull for startup/reconcile; live presence
/// normally comes from the owned presence watcher.
#[no_mangle]
pub extern "C" fn av_devices_list(
    engine: *mut AvEngine,
    out_json: *mut *mut c_char,
    error: *mut AvError,
) -> i32 {
    guard_error(error, || {
        let context = unsafe { engine_ref(engine) }?.context();
        let items = block(snapshot(context))?;
        out_str(out_json, &to_json(&items));
        Ok(())
    })
}

/// Reads the device battery state over a lockdown session.
async fn battery_inner(context: &EngineContext, udid: &str) -> Result<String, EngineFailure> {
    let provider = provider_for(context, udid).await?;
    let mut lc = authed_lockdown(&provider).await?;
    let value = lc.get_value(None, Some("com.apple.mobile.battery")).await?;
    let Value::Dictionary(d) = value else {
        let detail = "battery domain did not return a dictionary";
        return Err(EngineFailure::new(ErrorKind::Internal, detail));
    };
    let level = d
        .get("BatteryCurrentCapacity")
        .and_then(|v| v.as_unsigned_integer())
        .ok_or_else(|| EngineFailure::new(ErrorKind::Internal, "battery capacity is unavailable"))?
        as i32;
    let charging = d
        .get("ExternalConnected")
        .and_then(|v| v.as_boolean())
        .or_else(|| d.get("BatteryIsCharging").and_then(|v| v.as_boolean()))
        .unwrap_or(false);
    Ok(to_json(&BatteryOut { charging, level }))
}

#[derive(serde::Serialize)]
struct BatteryOut {
    charging: bool,
    level: i32,
}

/// Reads battery into `out_json`: {"charging":bool,"level":int}. Bounded — a
/// Wi-Fi lockdown session can hang.
#[no_mangle]
pub extern "C" fn av_device_battery(
    engine: *mut AvEngine,
    udid: *const c_char,
    out_json: *mut *mut c_char,
    error: *mut AvError,
) -> i32 {
    guard_error(error, || {
        let (engine, udid) = unsafe { engine_udid(engine, udid) }?;
        let json = block_bounded(
            timeouts::PROBE,
            "battery read timed out",
            battery_inner(engine.context(), &udid),
        )?;
        out_str(out_json, &json);
        Ok(())
    })
}

// ---- pairing: the guided wizard's engine half ------------------------------

#[derive(serde::Serialize)]
struct UsbItem {
    udid: String,
    name: String,
}

/// USB-reachable devices for the pairing wizard: [{"udid","name"}]. The name
/// comes from an unauthenticated lockdown GetValue (allowed pre-pairing); the
/// udid stands in when even that fails.
async fn usb_list_inner(context: &EngineContext) -> Result<Vec<UsbItem>, IdeviceError> {
    let mut items = Vec::new();
    for d in devices_deduped(context).await? {
        if !matches!(d.connection_type, Connection::Usb) {
            continue;
        }
        let name = match timeout(DISCOVERY_TIMEOUT, usb_name(context, &d)).await {
            Ok(Some(name)) => name,
            _ => d.udid.clone(),
        };
        items.push(UsbItem { udid: d.udid, name });
    }
    Ok(items)
}

async fn usb_name(context: &EngineContext, device: &UsbmuxdDevice) -> Option<String> {
    let provider = provider_from(context, device);
    let mut lc = LockdownClient::connect(&provider).await.ok()?;
    get_required_string(&mut lc, "DeviceName").await.ok()
}

/// Lists USB devices into out_json (for the pairing wizard).
#[no_mangle]
pub extern "C" fn av_usb_devices_list(
    engine: *mut AvEngine,
    out_json: *mut *mut c_char,
    error: *mut AvError,
) -> i32 {
    guard_error(error, || {
        let context = unsafe { engine_ref(engine) }?.context();
        let items = block(usb_list_inner(context))?;
        out_str(out_json, &to_json(&items));
        Ok(())
    })
}
