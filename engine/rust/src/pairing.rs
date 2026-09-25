//! Pairing lifecycle: trust (idevicepair-style, Wi-Fi-authorized on success),
//! unpair and the device-side backup-encryption password.

use std::ffi::c_char;
use std::time::Duration;

use idevice::pairing_file::PairingFile;
use idevice::services::lockdown::LockdownClient;
use idevice::services::mobilebackup2::MobileBackup2Client;
use idevice::{IdeviceError, IdeviceService};
use plist::Value;
use tokio_util::sync::CancellationToken;
use tracing::Instrument;

use crate::discover::read_will_encrypt;
use crate::engine_error::{EngineFailure, ErrorKind};
use crate::ffi::{block_bounded, engine_udid, guard_error, opt_owned, out_str, AvEngine, AvError};
use crate::logging::operation_span;
use crate::mobilebackup2;
use crate::pairing_store::{PairingIdentity, PairingStoreError};
use crate::provider::{
    authed_lockdown, block, provider_for, recv_framed, send_framed, AirvaultProvider, EngineContext,
};
use crate::timeouts;

const MAX_PASSWORD_DL_FRAME_BYTES: usize = 8 * 1024 * 1024;
/// ChangePassword waits on the passcode prompt iOS raises: a person at the
/// phone. iOS never expires the prompt, so this budget alone decides their time.
const PASSWORD_PROMPT_TIMEOUT: Duration = Duration::from_secs(180);
/// Pairing covers certificate generation, which is slow in debug shim builds.
/// Not a prompt budget: an unanswered trust dialog returns `trust_pending`.
const PAIRING_ADVANCE_TIMEOUT: Duration = Duration::from_secs(45);

fn outcome_unknown(detail: impl Into<String>) -> EngineFailure {
    EngineFailure::new(ErrorKind::OutcomeUnknown, detail)
}

#[derive(Debug)]
enum PasswordProtocolOutcome {
    Committed,
    Rejected(EngineFailure),
    Indeterminate(EngineFailure),
}

struct PasswordRunResult {
    encrypted: Option<bool>,
    result: Result<(), EngineFailure>,
}

/// The pinned idevice reader allocates an untrusted u32 frame length directly.
/// ChangePassword owns its receive loop, so enforce a bound before allocation.
async fn receive_capped_dl_message(
    mb2: &mut MobileBackup2Client,
) -> Result<(String, Value), String> {
    let body = recv_framed(&mut mb2.idevice, MAX_PASSWORD_DL_FRAME_BYTES)
        .await
        .map_err(|e| format!("{e:?}"))?;
    let value: Value = plist::from_bytes(&body).map_err(|e| format!("decode: {e}"))?;
    let tag = match &value {
        Value::Array(array) => array.first().and_then(Value::as_string),
        _ => None,
    }
    .ok_or_else(|| "invalid DeviceLink frame: expected array with string tag".to_string())?;
    Ok((tag.to_owned(), value))
}

/// Tells the phone to forget this host, best effort: `Unpair` is device-side
/// only, the host record goes either way, so the outcome is logged not returned.
/// InvalidHostID means it had already forgotten us.
async fn device_unpair(context: &EngineContext, udid: &str, pf: &PairingFile) {
    let attempt = async {
        let provider = provider_for(context, udid).await?;
        let mut lc = LockdownClient::connect(&provider).await?;
        // Over Wi-Fi Unpair only lands inside a session; over USB it is taken plain.
        let _ = lc.start_session(pf).await;
        lc.unpair(pf.host_id.clone()).await
    };
    match tokio::time::timeout(timeouts::CONNECT, attempt).await {
        Ok(Ok(())) | Ok(Err(IdeviceError::InvalidHostID)) => {
            tracing::info!(udid = %udid, "unpair: device forgot this host")
        }
        Ok(Err(e)) => tracing::warn!(
            udid = %udid,
            error = ?e,
            "unpair: device-side revoke not acknowledged; removing host state anyway"
        ),
        Err(_) => tracing::warn!(
            udid = %udid,
            "unpair: device did not answer in time; removing host state anyway"
        ),
    }
}

fn delete_local_pairing(context: &EngineContext, udid: &str) -> Result<(), String> {
    let record = context.pairing_store.delete_pairing(udid);
    let identity = context.pairing_store.delete_identity(udid);
    match (record, identity) {
        (Ok(_), Ok(_)) => Ok(()),
        (Err(record), Ok(_)) => Err(record.to_string()),
        (Ok(_), Err(identity)) => Err(identity.to_string()),
        (Err(record), Err(identity)) => Err(format!(
            "{}; also could not remove pending pairing identity: {}",
            record, identity
        )),
    }
}

/// Unpairs: lockdown `Unpair` so the phone forgets this host, then our host
/// record goes — independently, whatever the phone answered, so no device can
/// become unremovable. AV_ERROR_INTERNAL = that removal itself failed.
#[no_mangle]
pub extern "C" fn av_pairing_unpair(
    engine: *mut AvEngine,
    udid: *const c_char,
    error: *mut AvError,
) -> i32 {
    guard_error(error, || {
        let (engine, udid) = unsafe { engine_udid(engine, udid) }?;
        let context = engine.context();
        match context.pairing_store.load_pairing(&udid) {
            Ok(Some(pf)) => block(device_unpair(context, &udid, &pf)),
            Ok(None) => tracing::warn!(udid = %udid, "unpair: no local pairing record"),
            Err(PairingStoreError::InvalidUdid(_)) => {
                return Err(EngineFailure::invalid_argument("bad udid"));
            }
            Err(store) => tracing::warn!(
                udid = %udid,
                error = %store,
                "unpair: pairing record unreadable, cannot revoke on the device"
            ),
        }
        // Only OUR record is deleted: the muxer's system store is shared
        // per-device (Finder's record lives there on a dev Mac), and
        // removing it destroys the host's Wi-Fi visibility of the phone.
        delete_local_pairing(context, &udid)
            .map_err(|cleanup| EngineFailure::new(ErrorKind::Internal, cleanup))
    })
}

/// One pairing attempt, `idevicepair`-style: while the Trust dialog is up the
/// device answers Pending (no dialog stacking), and the attempt right after
/// the user taps Trust succeeds. The caller polls this until it settles.
async fn pair_trust_inner(
    context: &EngineContext,
    udid: &str,
) -> Result<&'static str, EngineFailure> {
    let mut mux = context.mux_addr().connect(0).await?;
    let provider = provider_for(context, udid).await?;
    let mut lc = LockdownClient::connect(&provider).await?;
    let pairing_store = &context.pairing_store;

    // Idempotent, but only OUR OWN record counts as already-paired — a Finder
    // record on a dev Mac must NOT stand in for an AirVault pairing.
    let existing = pairing_store.load_pairing(udid)?;
    let preferred_identity = if let Some(pf) = existing {
        match lc.start_session(&pf).await {
            Ok(_) => {
                pairing_store.delete_identity(udid)?;
                return Ok(finish_pairing(&mut mux, &mut lc, udid, pf).await);
            }
            Err(IdeviceError::InvalidHostID) => {}
            Err(IdeviceError::PasswordProtected | IdeviceError::DeviceLocked) => {
                return Ok("locked")
            }
            Err(e) => return Err(e.into()),
        }
        // The device explicitly rejected this record. Reuse its stable host
        // identity while creating fresh cryptographic material.
        lc = LockdownClient::connect(&provider).await?;
        Some(PairingIdentity::new(pf.host_id, pf.system_buid))
    } else {
        None
    };

    // Persist HostID/SystemBUID before the trust request: a pending dialog can
    // span many polls or a daemon restart, and rerolling the identifiers leaves
    // device and host disagreeing about which relationship the user approved.
    let identity = match pairing_store.load_identity(udid)? {
        Some(identity) => identity,
        None => {
            let candidate = match preferred_identity {
                Some(identity) => identity,
                None => PairingIdentity::new(
                    uuid::Uuid::new_v4().to_string().to_uppercase(),
                    mux.get_buid().await?,
                ),
            };
            pairing_store.persist_identity_if_absent(udid, candidate)?
        }
    };
    match lc
        .pair_once(
            identity.host_id.clone(),
            identity.system_buid.clone(),
            Some("AirVault"),
        )
        .await
    {
        Ok(pf) => {
            if pf.host_id != identity.host_id || pf.system_buid != identity.system_buid {
                return Err(EngineFailure::new(
                    ErrorKind::Internal,
                    "device returned a pairing record with different HostID/SystemBUID",
                ));
            }
            // Our own store is the source of truth. Wi-Fi setup below also
            // ensures that the muxer has a record it can use for discovery.
            pairing_store.save_pairing(udid, &pf)?;
            pairing_store.delete_identity(udid)?;
            if let Err(error) = lc.start_session(&pf).await {
                tracing::warn!(
                    udid,
                    ?error,
                    "paired over USB, but could not start the authenticated Wi-Fi setup session"
                );
                return Ok("wifi_authorization_failed");
            }
            Ok(finish_pairing(&mut mux, &mut lc, udid, pf).await)
        }
        Err(IdeviceError::PairingDialogResponsePending) => Ok("trust_pending"),
        Err(IdeviceError::UserDeniedPairing) => Ok("denied"),
        Err(IdeviceError::PasswordProtected) => Ok("locked"),
        Err(e) => Err(e.into()),
    }
}

/// Triggers/advances pairing. out_status: "paired" | "trust_pending" |
/// "denied" | "locked" | "wifi_authorization_failed".
#[no_mangle]
pub extern "C" fn av_pairing_advance(
    engine: *mut AvEngine,
    udid: *const c_char,
    job_id: *const c_char,
    out_status: *mut *mut c_char,
    error: *mut AvError,
) -> i32 {
    guard_error(error, || {
        let (engine, udid) = unsafe { engine_udid(engine, udid) }?;
        let job_id = unsafe { opt_owned(job_id) };
        let span = operation_span(&job_id, "pairing", &udid);
        let status = block_bounded(
            PAIRING_ADVANCE_TIMEOUT,
            "pairing timed out",
            pair_trust_inner(engine.context(), &udid).instrument(span),
        )?;
        out_str(out_status, status);
        Ok(())
    })
}

/// Pairing completes only once the muxer holds a discovery record and the
/// device acknowledges the wireless-lockdown write. Failure keeps the saved USB
/// record, so a retry re-enters the existing-record branch with no Trust prompt.
async fn finish_pairing(
    mux: &mut idevice::usbmuxd::UsbmuxdConnection,
    lockdown: &mut LockdownClient,
    udid: &str,
    pairing: PairingFile,
) -> &'static str {
    let setup = async {
        match mux.get_pair_record(udid).await {
            Ok(_) => tracing::info!(udid, "pairing: muxer pairing record already present"),
            Err(read_error) => {
                let serialized = pairing.serialize().map_err(|error| {
                    format!("muxer pairing record serialization failed: {error} [{error:?}]")
                })?;
                mux.save_pair_record(udid, serialized)
                    .await
                    .map_err(|save_error| {
                        format!(
                            "muxer pairing record unavailable: {read_error} [{read_error:?}]; SavePairRecord failed: {save_error} [{save_error:?}]"
                        )
                    })?;
                tracing::info!(
                    udid,
                    read_error = ?read_error,
                    "pairing: muxer pairing record saved"
                );
            }
        }

        lockdown
            .set_value(
                "EnableWifiConnections",
                Value::Boolean(true),
                Some("com.apple.mobile.wireless_lockdown"),
            )
            .await
            .map_err(|error| {
                format!("EnableWifiConnections SetValue failed: {error} [{error:?}]")
            })?;
        tracing::info!(udid, "pairing: Wi-Fi sync authorization acknowledged");
        Ok::<(), String>(())
    }
    .await;

    match setup {
        Ok(()) => "paired",
        Err(error) => {
            tracing::warn!(
                udid,
                error = %error,
                "paired over USB, but could not complete Wi-Fi setup"
            );
            "wifi_authorization_failed"
        }
    }
}

/// Sets/changes/removes the device-side backup password via mobilebackup2
/// ChangePassword (None old = enable, None new = disable). Driven by hand:
/// upstream's `change_password_from_path` discards the wrong-password verdict.
async fn backup_password_run(
    context: &EngineContext,
    udid: &str,
    old: Option<&str>,
    new: Option<&str>,
    cancel: CancellationToken,
) -> PasswordRunResult {
    if cancel.is_cancelled() {
        return PasswordRunResult {
            encrypted: None,
            result: Err(EngineFailure::new(
                ErrorKind::Cancelled,
                "backup password change cancelled before it started",
            )),
        };
    }
    let provider = match provider_for(context, udid).await {
        Ok(provider) => provider,
        Err(error) => {
            return PasswordRunResult {
                encrypted: None,
                result: Err(EngineFailure::from_idevice(
                    "backup password provider lookup failed",
                    error,
                )),
            }
        }
    };

    // The baseline prevents a stale UI from treating an already-matching flag
    // as proof that this request changed it. Only a transition can rescue an
    // otherwise indeterminate enable/disable operation.
    let baseline = probe_backup_encryption(&provider).await;
    tracing::debug!(encrypted = ?baseline, "backup password: captured encryption baseline");
    // ChangePassword is standalone: unlike backup/restore, it takes no AFC sync lock.
    let mut mb2 =
        match tokio::time::timeout(timeouts::CONNECT, mobilebackup2::connect(&provider)).await {
            Ok(Ok(client)) => client,
            Ok(Err(error)) => {
                return PasswordRunResult {
                    encrypted: baseline,
                    result: Err(EngineFailure::from_idevice(
                        "backup password mobilebackup2 connect failed",
                        error,
                    )),
                }
            }
            Err(_) => {
                return PasswordRunResult {
                    encrypted: baseline,
                    result: Err(EngineFailure::new(
                        ErrorKind::Timeout,
                        format!(
                            "backup password mobilebackup2 connect timed out after {}s",
                            timeouts::CONNECT.as_secs()
                        ),
                    )),
                }
            }
        };

    // Last point with no device-side effect: past here a cancel can only be
    // reported as an indeterminate outcome.
    if cancel.is_cancelled() {
        return PasswordRunResult {
            encrypted: baseline,
            result: Err(EngineFailure::new(
                ErrorKind::Cancelled,
                "backup password change cancelled before request",
            )),
        };
    }

    if let Err(failure) = send_backup_password_request(&mut mb2, udid, old, new).await {
        return PasswordRunResult {
            encrypted: baseline,
            result: Err(failure),
        };
    }
    tracing::debug!("backup password: ChangePassword request sent");

    let expected_transition =
        expected_encryption_transition(baseline, old.is_some(), new.is_some());
    let outcome = {
        let protocol = read_password_device_link(&mut mb2);
        let state_transition = wait_backup_encryption_transition(&provider, expected_transition);
        tokio::pin!(protocol);
        tokio::pin!(state_transition);

        tokio::select! {
            biased;
            outcome = &mut protocol => outcome,
            encrypted = &mut state_transition => {
                tracing::debug!(encrypted, "backup password: encryption postcondition changed");
                PasswordProtocolOutcome::Committed
            }
            _ = tokio::time::sleep(PASSWORD_PROMPT_TIMEOUT) => {
                PasswordProtocolOutcome::Indeterminate(outcome_unknown(format!(
                    "backup password request outcome is unknown after {}s",
                    PASSWORD_PROMPT_TIMEOUT.as_secs()
                )))
            }
            _ = cancel.cancelled() => {
                PasswordProtocolOutcome::Indeterminate(outcome_unknown(
                    "backup password request was cancelled after it reached the device; final outcome is unknown"
                ))
            }
        }
    };

    // Close the operation channel before opening the final lockdown probe.
    drop(mb2);
    let final_state = probe_backup_encryption(&provider).await.or(baseline);

    match outcome {
        PasswordProtocolOutcome::Committed => {
            let encrypted = match (old, new) {
                (None, Some(_)) => Some(true),
                (Some(_), None) => Some(false),
                (Some(_), Some(_)) => final_state.or(Some(true)),
                _ => final_state,
            };
            tracing::info!(encrypted = ?encrypted, "backup password change committed");
            PasswordRunResult {
                encrypted,
                result: Ok(()),
            }
        }
        PasswordProtocolOutcome::Rejected(failure) => PasswordRunResult {
            encrypted: final_state,
            result: Err(failure),
        },
        PasswordProtocolOutcome::Indeterminate(failure) => {
            if let Some(expected) = expected_transition {
                if final_state == Some(expected) {
                    tracing::info!(
                        encrypted = expected,
                        "backup password change committed: encryption state reached the expected value"
                    );
                    return PasswordRunResult {
                        encrypted: Some(expected),
                        result: Ok(()),
                    };
                }
            }
            PasswordRunResult {
                encrypted: final_state,
                result: Err(failure),
            }
        }
    }
}

/// A WillEncrypt flip proves the state changed, not that this request changed
/// it — another host can race us; accepted as the best available evidence.
fn expected_encryption_transition(
    baseline: Option<bool>,
    has_old_password: bool,
    has_new_password: bool,
) -> Option<bool> {
    match (has_old_password, has_new_password) {
        (false, true) if baseline == Some(false) => Some(true),
        (true, false) if baseline == Some(true) => Some(false),
        _ => None,
    }
}

async fn send_backup_password_request(
    mb2: &mut MobileBackup2Client,
    udid: &str,
    old: Option<&str>,
    new: Option<&str>,
) -> Result<(), EngineFailure> {
    // The ChangePassword message: passwords live at the TOP level of the
    // message dict (not under Options), exactly as iTunes sends it.
    let mut msg = plist::Dictionary::new();
    msg.insert("MessageName".into(), Value::String("ChangePassword".into()));
    msg.insert("TargetIdentifier".into(), Value::String(udid.into()));
    if let Some(old) = old {
        msg.insert("OldPassword".into(), Value::String(old.into()));
    }
    if let Some(new) = new {
        msg.insert("NewPassword".into(), Value::String(new.into()));
    }
    let dl = Value::Array(vec![
        Value::String("DLMessageProcessMessage".into()),
        Value::Dictionary(msg),
    ]);
    // Device-link framing: u32 BE length + binary plist, over the raw socket.
    let mut body = Vec::new();
    plist::to_writer_binary(&mut body, &dl)
        .map_err(|error| EngineFailure::new(ErrorKind::Protocol, format!("encode: {error}")))?;
    send_framed(&mut mb2.idevice, &body)
        .await
        .map_err(|error| EngineFailure::from_idevice("send ChangePassword failed", error))
}

async fn read_password_device_link(mb2: &mut MobileBackup2Client) -> PasswordProtocolOutcome {
    // This one future owns the DeviceLink reader for the full message loop, so
    // no competing select branch can cancel a partially-read frame.
    loop {
        let (tag, value) = match receive_capped_dl_message(mb2).await {
            Ok(message) => message,
            Err(detail) => {
                return PasswordProtocolOutcome::Indeterminate(outcome_unknown(format!(
                    "password change result is unknown after DeviceLink read failed: {detail}"
                )))
            }
        };
        if let Some(outcome) = password_change_verdict(&tag, &value) {
            return outcome;
        }
        // Match idevicebackup2: unknown/intermediate messages are ignored.
        tracing::debug!(
            tag,
            "backup password: ignoring intermediate DeviceLink message"
        );
    }
}

async fn read_backup_encryption(provider: &AirvaultProvider) -> Result<bool, IdeviceError> {
    let mut lockdown = authed_lockdown(provider).await?;
    read_will_encrypt(&mut lockdown).await
}

/// None = the flag could not be read. Failures are expected while the phone is
/// busy applying the change, so they stay at debug.
async fn probe_backup_encryption(provider: &AirvaultProvider) -> Option<bool> {
    match tokio::time::timeout(timeouts::PROBE, read_backup_encryption(provider)).await {
        Ok(Ok(encrypted)) => Some(encrypted),
        Ok(Err(error)) => {
            tracing::debug!(%error, ?error, "backup password encryption-state probe failed");
            None
        }
        Err(_) => {
            tracing::debug!("backup password encryption-state probe timed out");
            None
        }
    }
}

async fn wait_backup_encryption_transition(
    provider: &AirvaultProvider,
    expected: Option<bool>,
) -> bool {
    let Some(expected) = expected else {
        return std::future::pending().await;
    };
    loop {
        if probe_backup_encryption(provider).await == Some(expected) {
            return expected;
        }
        tokio::time::sleep(timeouts::POLL_INTERVAL).await;
    }
}

/// Returns `Some` only for terminal ChangePassword messages. A disconnect has
/// no explicit device result, so the operation's outcome is unknown, not Ok.
fn password_change_verdict(tag: &str, value: &Value) -> Option<PasswordProtocolOutcome> {
    const FALLBACK: &str = "the device rejected the password change";

    match tag {
        "DLMessageProcessMessage" => {
            let dict = match value {
                Value::Array(arr) => arr.get(1).and_then(|v| v.as_dictionary()).cloned(),
                _ => None,
            };
            Some(match mobilebackup2::verdict(dict, FALLBACK) {
                Ok(()) => PasswordProtocolOutcome::Committed,
                Err(error) if error.code.is_some() => {
                    tracing::info!(
                        error_code = ?error.code,
                        "backup password change rejected by device"
                    );
                    PasswordProtocolOutcome::Rejected(EngineFailure::from(error))
                }
                Err(error) => PasswordProtocolOutcome::Indeterminate(outcome_unknown(format!(
                    "password change result is unknown: {}",
                    error.detail
                ))),
            })
        }
        "DLMessageDisconnect" => Some(PasswordProtocolOutcome::Indeterminate(outcome_unknown(
            "password change result is unknown: device disconnected before a final verdict",
        ))),
        _ => None,
    }
}

/// Backup-password management: empty old_pw enables encryption, empty new_pw
/// disables it, both set change it; the long bound covers the device-passcode
/// wait. out_encrypted (-1 = unknown, else the device flag) is set even on error.
#[no_mangle]
pub extern "C" fn av_backup_password_change(
    engine: *mut AvEngine,
    udid: *const c_char,
    job_id: *const c_char,
    old_pw: *const c_char,
    new_pw: *const c_char,
    out_encrypted: *mut i32,
    error: *mut AvError,
) -> i32 {
    guard_error(error, || {
        if !out_encrypted.is_null() {
            unsafe { *out_encrypted = -1 };
        }
        let (engine, udid) = unsafe { engine_udid(engine, udid) }?;
        let context = engine.context();
        let job_id = unsafe { opt_owned(job_id) };
        if job_id.is_empty() {
            return Err(EngineFailure::invalid_argument(
                "backup password change requires a job id",
            ));
        }
        let lease = engine.operations.register_command(job_id.clone());
        let old = unsafe { opt_owned(old_pw) };
        let new = unsafe { opt_owned(new_pw) };
        let span = operation_span(&job_id, "password", &udid);
        block(
            async {
                let old = (!old.is_empty()).then_some(old.as_str());
                let new = (!new.is_empty()).then_some(new.as_str());
                let outcome =
                    backup_password_run(context, &udid, old, new, lease.cancellation_token()).await;
                if let Some(encrypted) = outcome.encrypted {
                    if !out_encrypted.is_null() {
                        unsafe { *out_encrypted = i32::from(encrypted) };
                    }
                }
                outcome.result
            }
            .instrument(span),
        )
    })
}

#[cfg(test)]
mod password_tests {
    use plist::{Dictionary, Value};

    use super::{expected_encryption_transition, password_change_verdict, PasswordProtocolOutcome};
    use crate::engine_error::ErrorKind;

    fn process_message(code: i64) -> Value {
        let mut verdict = Dictionary::new();
        verdict.insert("ErrorCode".into(), Value::Integer(code.into()));
        verdict.insert(
            "ErrorDescription".into(),
            Value::String("device detail".into()),
        );
        Value::Array(vec![
            Value::String("DLMessageProcessMessage".into()),
            Value::Dictionary(verdict),
        ])
    }

    #[test]
    fn password_change_commits_only_on_a_zero_verdict() {
        let outcome =
            |code| password_change_verdict("DLMessageProcessMessage", &process_message(code));
        assert!(matches!(
            outcome(0),
            Some(PasswordProtocolOutcome::Committed)
        ));
        assert!(matches!(
            outcome(207),
            Some(PasswordProtocolOutcome::Rejected(failure))
                if failure.kind == ErrorKind::InvalidBackupPassword
        ));
    }

    // Without a readable verdict the device may or may not have changed the
    // password, so neither success nor rejection may be reported.
    #[test]
    fn unreadable_final_message_is_indeterminate() {
        let disconnect = Value::Array(vec![Value::String("DLMessageDisconnect".into())]);
        assert!(matches!(
            password_change_verdict("DLMessageDisconnect", &disconnect),
            Some(PasswordProtocolOutcome::Indeterminate(failure))
                if failure.kind == ErrorKind::OutcomeUnknown
        ));
        let malformed = Value::Array(vec![
            Value::String("DLMessageProcessMessage".into()),
            Value::Dictionary(Dictionary::new()),
        ]);
        assert!(matches!(
            password_change_verdict("DLMessageProcessMessage", &malformed),
            Some(PasswordProtocolOutcome::Indeterminate(_))
        ));
    }

    #[test]
    fn state_postcondition_requires_a_real_transition() {
        assert_eq!(
            expected_encryption_transition(Some(false), false, true),
            Some(true)
        );
        assert_eq!(
            expected_encryption_transition(Some(true), true, false),
            Some(false)
        );
        assert_eq!(
            expected_encryption_transition(Some(true), false, true),
            None
        );
        assert_eq!(expected_encryption_transition(None, false, true), None);
        assert_eq!(expected_encryption_transition(Some(true), true, true), None);
    }
}
