//! Discovery & cheap device reads: enriched discover, the availability
//! snapshot shared with the watcher, battery and the USB list.

use std::ffi::c_char;
use std::time::Duration;

use idevice::provider::IdeviceProvider;
use idevice::services::lockdown::LockdownClient;
use idevice::usbmuxd::Connection;
use idevice::IdeviceService;
use plist::Value;

use crate::engine_error::ErrorKind;
use crate::ffi::{engine_ref, engine_udid, guard_error, AvEngine, AvError};
use crate::{
    block, block_probe, conn_str, devices_deduped, getv_str, out_str, provider_for, to_json,
    write_err, EngineContext, ProbeError,
};

#[derive(serde::Serialize)]
struct SnapshotItem {
    udid: String,
    connection: &'static str,
}

/// The cheap availability snapshot: usbmuxd get_devices with NO lockdown
/// metadata, as `[{"udid","connection"}]`. Shared by the one-shot list and the
/// owned presence watcher so both speak the identical shape.
pub(crate) async fn snapshot_json(
    context: &EngineContext,
) -> Result<String, idevice::IdeviceError> {
    let devices = devices_deduped(context).await?;
    let items: Vec<SnapshotItem> = devices
        .into_iter()
        .map(|d| SnapshotItem {
            connection: conn_str(&d.connection_type),
            udid: d.udid,
        })
        .collect();
    Ok(to_json(&items))
}

/// The lockdown-enriched view of a device: identity plus pairing/sync/encryption
/// flags. Pairing is intentionally tri-state: a transport or lockdown failure
/// does not prove that a previously paired device became unpaired.
#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize)]
#[serde(rename_all = "lowercase")]
enum PairingState {
    Paired,
    Unpaired,
    Unknown,
}

struct DeviceMeta {
    name: String,
    ptype: String,
    ver: String,
    metadata_known: bool,
    pairing_state: PairingState,
    flags_known: bool,
    enc: bool,
    activation: String,
}

impl DeviceMeta {
    fn unknown() -> Self {
        Self {
            name: String::new(),
            ptype: String::new(),
            ver: String::new(),
            metadata_known: false,
            pairing_state: PairingState::Unknown,
            flags_known: false,
            enc: false,
            activation: String::new(),
        }
    }
}

/// Enrich a device over lockdown, time-bounded. Failure produces an explicit
/// unknown result instead of removing a mux-reachable device from the response.
async fn device_meta(context: &EngineContext, udid: &str) -> DeviceMeta {
    let deadline = tokio::time::Instant::now() + Duration::from_secs(4);
    let provider = match tokio::time::timeout_at(deadline, provider_for(context, udid)).await {
        Ok(Ok(provider)) => provider,
        Ok(Err(_)) | Err(_) => return DeviceMeta::unknown(),
    };
    let mut lc = match tokio::time::timeout_at(deadline, LockdownClient::connect(&provider)).await {
        Ok(Ok(client)) => client,
        Ok(Err(_)) | Err(_) => return DeviceMeta::unknown(),
    };

    // Establish the pairing verdict first. Once a session succeeds, later
    // metadata or flag timeouts must not erase that confirmed result.
    let pairing_state = pairing_state(&provider, &mut lc, deadline).await;

    // Identity is an all-or-nothing snapshot. Consumers must not overwrite
    // known metadata with empty strings when any GetValue read fails.
    let identity = tokio::time::timeout_at(deadline, read_identity(&mut lc))
        .await
        .unwrap_or_default();

    // A failed or malformed read is propagated to flagsKnown=false; a valid
    // boolean false remains distinguishable.
    let (enc, flags_known) = if pairing_state == PairingState::Paired {
        // The encryption flag does not affect the already-confirmed pairing verdict.
        match tokio::time::timeout_at(deadline, crate::read_will_encrypt(&mut lc)).await {
            Ok(Ok(enc)) => (enc, true),
            Ok(Err(_)) | Err(_) => (false, false),
        }
    } else {
        (false, false)
    };

    // Best-effort: an unactivated phone (Setup Assistant) cannot run mb2
    // operations, so the UI warns from this field. Empty = unknown.
    let activation =
        match tokio::time::timeout_at(deadline, get_required_string(&mut lc, "ActivationState"))
            .await
        {
            Ok(Ok(state)) => state,
            Ok(Err(_)) | Err(_) => String::new(),
        };

    let (name, ptype, ver, metadata_known) = match identity {
        Some((name, ptype, ver)) => (name, ptype, ver, true),
        None => (String::new(), String::new(), String::new(), false),
    };

    DeviceMeta {
        name,
        ptype,
        ver,
        metadata_known,
        pairing_state,
        flags_known,
        enc,
        activation,
    }
}

async fn pairing_state(
    provider: &crate::AirvaultProvider,
    lc: &mut LockdownClient,
    deadline: tokio::time::Instant,
) -> PairingState {
    // A missing local pairing record surfaces as InvalidHostID from the
    // provider; the same variant from start_session means the phone revoked us.
    let pf = match provider.get_pairing_file().await {
        Ok(pf) => pf,
        Err(error) => return unpaired_or_unknown(&error),
    };

    match tokio::time::timeout_at(deadline, lc.start_session(&pf)).await {
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

async fn get_required_string(
    lc: &mut LockdownClient,
    key: &str,
) -> Result<String, idevice::IdeviceError> {
    lc.get_value(Some(key), None)
        .await?
        .as_string()
        .filter(|value| !value.is_empty())
        .map(str::to_owned)
        .ok_or_else(|| {
            idevice::IdeviceError::UnexpectedResponse(format!(
                "lockdown {key} is missing or not a non-empty string"
            ))
        })
}

fn unpaired_or_unknown(error: &idevice::IdeviceError) -> PairingState {
    match error {
        idevice::IdeviceError::InvalidHostID => PairingState::Unpaired,
        _ => PairingState::Unknown,
    }
}

#[derive(serde::Serialize)]
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

/// Discovers reachable devices WITH lockdown metadata (see DiscoverItem).
/// No connection field on purpose — presence/transport is the watcher's domain.
#[no_mangle]
pub extern "C" fn av_devices_inspect(
    engine: *mut AvEngine,
    out_json: *mut *mut c_char,
    error: *mut AvError,
) -> i32 {
    guard_error(error, |err| {
        let Some(engine) = (unsafe { engine_ref(engine, err) }) else {
            return ErrorKind::InvalidArgument.code();
        };
        let context = engine.context();
        block(async {
            let devices = match devices_deduped(context).await {
                Ok(d) => d,
                Err(e) => return write_err(err, &e),
            };

            let mut items: Vec<DiscoverItem> = Vec::with_capacity(devices.len());
            for d in devices {
                let meta = device_meta(context, &d.udid).await;
                items.push(DiscoverItem {
                    name: if meta.name.is_empty() {
                        d.udid.clone()
                    } else {
                        meta.name
                    },
                    udid: d.udid,
                    product_type: meta.ptype,
                    ios_version: meta.ver,
                    pairing_state: meta.pairing_state,
                    metadata_known: meta.metadata_known,
                    flags_known: meta.flags_known,
                    encrypted: meta.enc,
                    activation_state: meta.activation,
                });
            }
            out_str(out_json, &to_json(&items));
            0
        })
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
    guard_error(error, |err| {
        let Some(engine) = (unsafe { engine_ref(engine, err) }) else {
            return ErrorKind::InvalidArgument.code();
        };
        let context = engine.context();
        block(async {
            match snapshot_json(context).await {
                Ok(j) => {
                    out_str(out_json, &j);
                    0
                }
                Err(e) => write_err(err, &e),
            }
        })
    })
}

/// Reads the device battery state: (charging, level 0-100) over a lockdown
/// session. A failure to establish the session (the phone is gone) is
/// Unreachable; a session that answers with no usable capacity is Logical.
async fn battery_inner(context: &EngineContext, udid: &str) -> Result<(bool, i32), ProbeError> {
    let provider = provider_for(context, udid)
        .await
        .map_err(|e| ProbeError::Unreachable(format!("{e:?}")))?;
    let mut lc = crate::authed_lockdown(&provider)
        .await
        .map_err(|e| ProbeError::Unreachable(format!("{e:?}")))?;
    let value = lc
        .get_value(None, Some("com.apple.mobile.battery"))
        .await
        .map_err(|e| ProbeError::Unreachable(format!("{e:?}")))?;
    let Value::Dictionary(d) = value else {
        return Err(ProbeError::Logical(
            "battery domain did not return a dictionary".into(),
        ));
    };
    // A missing capacity means the phone answered oddly, not that it is gone.
    let level = d
        .get("BatteryCurrentCapacity")
        .and_then(|v| v.as_unsigned_integer())
        .ok_or_else(|| ProbeError::Logical("battery capacity is unavailable".into()))?
        as i32;
    let charging = d
        .get("ExternalConnected")
        .and_then(|v| v.as_boolean())
        .or_else(|| d.get("BatteryIsCharging").and_then(|v| v.as_boolean()))
        .unwrap_or(false);
    Ok((charging, level))
}

#[derive(serde::Serialize)]
struct BatteryOut {
    charging: bool,
    level: i32,
}

/// Reads battery into `out_json`: {"charging":bool,"level":int}. Bounded — a
/// Wi-Fi lockdown session can hang. A session-establish failure or the timeout
/// returns AV_ERROR_DEVICE_UNAVAILABLE so the caller can drive presence.
#[no_mangle]
pub extern "C" fn av_device_battery(
    engine: *mut AvEngine,
    udid: *const c_char,
    out_json: *mut *mut c_char,
    error: *mut AvError,
) -> i32 {
    guard_error(error, |err| {
        let Some((engine, udid)) = (unsafe { engine_udid(engine, udid, err) }) else {
            return ErrorKind::InvalidArgument.code();
        };
        let context = engine.context();
        block_probe(
            err,
            Duration::from_secs(5),
            "battery read timed out",
            async move {
                battery_inner(context, &udid)
                    .await
                    .map(|(charging, level)| to_json(&BatteryOut { charging, level }))
            },
            |payload| {
                out_str(out_json, &payload);
                0
            },
        )
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
async fn usb_list_inner(context: &EngineContext) -> Result<String, idevice::IdeviceError> {
    let devices = devices_deduped(context).await?;
    let mut items: Vec<UsbItem> = Vec::new();
    for d in devices {
        if !matches!(d.connection_type, Connection::Usb) {
            continue;
        }
        let name =
            match tokio::time::timeout(Duration::from_secs(3), usb_name(context, &d.udid)).await {
                Ok(n) if !n.is_empty() => n,
                _ => d.udid.clone(),
            };
        items.push(UsbItem { udid: d.udid, name });
    }
    Ok(to_json(&items))
}

async fn usb_name(context: &EngineContext, udid: &str) -> String {
    let provider = match provider_for(context, udid).await {
        Ok(p) => p,
        Err(_) => return String::new(),
    };
    let mut lc = match LockdownClient::connect(&provider).await {
        Ok(c) => c,
        Err(_) => return String::new(),
    };
    getv_str(&mut lc, "DeviceName").await
}

/// Lists USB devices into out_json (for the pairing wizard).
#[no_mangle]
pub extern "C" fn av_usb_devices_list(
    engine: *mut AvEngine,
    out_json: *mut *mut c_char,
    error: *mut AvError,
) -> i32 {
    guard_error(error, |err| {
        let Some(engine) = (unsafe { engine_ref(engine, err) }) else {
            return ErrorKind::InvalidArgument.code();
        };
        let context = engine.context();
        block(async {
            match usb_list_inner(context).await {
                Ok(j) => {
                    out_str(out_json, &j);
                    0
                }
                Err(e) => write_err(err, &e),
            }
        })
    })
}
