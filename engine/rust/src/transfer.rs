//! Cancellable mobilebackup2 backup/restore orchestration.

use std::ffi::c_char;
use std::future::Future;
use std::path::Path;
use std::time::Duration;

use tokio_util::sync::CancellationToken;

use idevice::services::mobilebackup2::RestoreOptions;
use idevice::IdeviceError;
use tracing::Instrument;

mod metadata;
mod sync_session;

use metadata::{
    prepare_backup_info, remove_restore_applications, stage_restore_applications,
    RestoreApplicationsError,
};
use sync_session::SyncSession;

use crate::backup_storage::{BackupCb, BackupStorage, ProgressSink, AV_BACKUP_PHASE_FINALIZING};
use crate::bounded::{self, cancel_or_timeout, Interrupt};
use crate::engine_error::{EngineFailure, ErrorKind};
use crate::ffi::{engine_udid, guard_error, in_str, req_str, AvEngine, AvError};
use crate::logging::operation_span;
use crate::mobilebackup2;
use crate::object_store::ObjectSession;
use crate::operation_registry::RegisterError;
use crate::path_sandbox::PathSandbox;
use crate::provider::{authed_lockdown, provider_for, AirvaultProvider, EngineContext};
use crate::timeouts;

// The Info.plist pass runs an app census of one icon round-trip per installed app.
const INFO_PLIST_TIMEOUT: Duration = Duration::from_secs(180);

fn merge_transfer_cleanup<T>(
    primary: Result<T, EngineFailure>,
    cleanup: Result<(), String>,
    stage: &str,
) -> Result<T, EngineFailure> {
    match (primary, cleanup) {
        (Ok(value), Ok(())) => Ok(value),
        (Err(primary), Ok(())) => Err(primary),
        // The device verdict stands (a restore may already be applied and
        // rebooting); a teardown hiccup must not discard it.
        (Ok(value), Err(cleanup)) => {
            tracing::warn!(error = %cleanup, "{stage} failed after success");
            Ok(value)
        }
        (Err(primary), Err(cleanup)) => Err(primary.with_cleanup(cleanup)),
    }
}

/// Awaits one transfer stage under `limit`; a cancellation or the elapsed
/// limit becomes that stage's failure.
async fn run_stage<T>(
    cancel: &CancellationToken,
    operation: &str,
    stage: &str,
    limit: Duration,
    future: impl Future<Output = T>,
) -> Result<T, EngineFailure> {
    cancel_or_timeout(cancel, limit, future)
        .await
        .map_err(|interrupt| match interrupt {
            Interrupt::Cancelled => EngineFailure::new(
                ErrorKind::Cancelled,
                format!("{operation} cancelled during {stage}"),
            ),
            Interrupt::TimedOut => EngineFailure::new(
                ErrorKind::Timeout,
                format!("{operation} {stage} timed out after {}s", limit.as_secs()),
            ),
        })
}

/// A recorded object-store failure is the root cause of whatever the transfer
/// reported after it, so it replaces that result.
fn prefer_store_error<T>(
    session: &ObjectSession,
    result: Result<T, EngineFailure>,
) -> Result<T, EngineFailure> {
    match session.error() {
        Some(error) => Err(EngineFailure::from(error)),
        None => result,
    }
}

/// Finder refuses to restore while Find My iPhone is on; the device would
/// reject with MBErrorDomain/211 anyway, so fail fast with the same
/// classification. Read failures never block — the device stays the authority.
async fn ensure_find_my_disabled(provider: &AirvaultProvider) -> Result<(), EngineFailure> {
    let read = bounded::within(timeouts::CONNECT, async {
        let mut lc = authed_lockdown(provider).await?;
        lc.get_value(Some("IsAssociated"), Some("com.apple.fmip"))
            .await
    });
    match read.await {
        Ok(value) if value.as_boolean() == Some(true) => Err(EngineFailure::new(
            ErrorKind::FindMyEnabled,
            "Find My iPhone is on; turn it off on the phone before restoring",
        )),
        Ok(_) => Ok(()),
        Err(error) => {
            tracing::debug!(error = ?error, "Find My preflight unavailable; the device enforces");
            Ok(())
        }
    }
}

/// One valid transfer configuration. Keeping operation and store selection in
/// the same enum makes mismatched backup/restore combinations unrepresentable.
enum TransferSpec {
    Backup {
        snapshot_id: String,
        base_snapshot_id: Option<String>,
    },
    Restore {
        snapshot_id: String,
        options: RestoreOptions,
    },
}

impl TransferSpec {
    fn label(&self) -> &'static str {
        match self {
            Self::Backup { .. } => "backup",
            Self::Restore { .. } => "restore",
        }
    }

    fn is_backup(&self) -> bool {
        matches!(self, Self::Backup { .. })
    }
}

struct Mb2Storage<'a> {
    target_udid: &'a str,
    source: &'a str,
    root: &'a Path,
    sandbox: PathSandbox,
}

/// Runs one mobilebackup2 transfer against `udid`. `source` owns the selected
/// object tree and may differ from the target during a phone migration.
/// A backup resolves to the payload bytes it added to the pool; a restore to 0.
async fn run_mb2(
    context: &EngineContext,
    udid: &str,
    source: &str,
    spec: TransferSpec,
    progress: ProgressSink,
    cancel: CancellationToken,
) -> Result<u64, EngineFailure> {
    // Idle AFC connections are useless during a multi-minute transfer, and a
    // restore reboots the phone.
    context.afc_pool.forget(udid);
    let root = context.backup_root();
    let is_backup = spec.is_backup();
    let sandbox = PathSandbox::new(root, source).map_err(EngineFailure::integrity)?;
    let provider = provider_for(context, udid)
        .await
        .map_err(|error| EngineFailure::from_idevice("device provider lookup failed", error))?;
    if !is_backup {
        ensure_find_my_disabled(&provider).await?;
    }
    // Announce a real sync session first — locked backups depend on it. All
    // later exits merge their primary result with bounded session cleanup.
    let sync = SyncSession::open(&provider, udid).await?;
    let storage = Mb2Storage {
        target_udid: udid,
        source,
        root,
        sandbox,
    };
    let transfer = run_mb2_transfer(&provider, storage, spec, progress, cancel.clone()).await;
    let cleanup = sync.finish().await;
    let session = merge_transfer_cleanup(transfer, cleanup, "sync session teardown")?;
    if is_backup {
        progress.emit(AV_BACKUP_PHASE_FINALIZING, -1.0, 0);
        // finish() refuses a cancelled token itself; run_transfer_export then
        // reports the failure as Cancelled.
        let added = session.finish(&cancel).map_err(EngineFailure::from)?;
        // A cancel that raced the manifest pass discards the sealed backup.
        if cancel.is_cancelled() {
            return Err(EngineFailure::new(ErrorKind::Cancelled, "backup cancelled"));
        }
        return Ok(added);
    }
    // A restore has no post-transfer step: reaching here means the device
    // already accepted it (an in-flight cancel would have errored the transfer
    // above), so it is applied and irreversible — never report a late cancel.
    Ok(0)
}

async fn run_mb2_transfer(
    provider: &AirvaultProvider,
    storage: Mb2Storage<'_>,
    spec: TransferSpec,
    progress: ProgressSink,
    cancel: CancellationToken,
) -> Result<ObjectSession, EngineFailure> {
    let Mb2Storage {
        target_udid: udid,
        source,
        root,
        sandbox,
    } = storage;
    let label = spec.label();
    let is_backup = spec.is_backup();
    let session = match &spec {
        TransferSpec::Backup {
            snapshot_id,
            base_snapshot_id,
        } => ObjectSession::backup(root, source, snapshot_id, base_snapshot_id.as_deref())
            .map_err(EngineFailure::from)?,
        TransferSpec::Restore { snapshot_id, .. } => {
            ObjectSession::restore(root, source, snapshot_id).map_err(EngineFailure::from)?
        }
    };
    // Set while RestoreApplications.plist may be on the device: a failed
    // restore must not leave it there (idevicebackup2 parity).
    let mut restore_apps_staged = false;
    let result = async {
        match &spec {
            // Refresh Info.plist (device identity + app census) before backing up.
            TransferSpec::Backup { .. } => {
                let prepare = prepare_backup_info(provider, udid, &session);
                let stage = "Info.plist preparation";
                let written = run_stage(&cancel, label, stage, INFO_PLIST_TIMEOUT, prepare).await?;
                prefer_store_error(
                    &session,
                    written.map_err(|error| {
                        EngineFailure::integrity(format!(
                            "backup: required Info.plist could not be written: {error}"
                        ))
                    }),
                )?;
            }
            // Stage the app-reinstall list on the device. Continuing without it
            // would silently restore with no App Store apps, so failure aborts
            // (idevicebackup2 parity).
            TransferSpec::Restore { .. } => {
                let prepare = stage_restore_applications(provider, &session);
                let stage = "RestoreApplications.plist preparation";
                let staged = run_stage(&cancel, label, stage, timeouts::DEVICE_WORK, prepare).await;
                // An empty app list or a snapshot-side failure never touches
                // the device; any other outcome may have staged it, if partially.
                restore_apps_staged = !matches!(
                    staged,
                    Ok(Ok(false) | Err(RestoreApplicationsError::Snapshot(_)))
                );
                match staged? {
                    Ok(_) => {}
                    Err(RestoreApplicationsError::Snapshot(error)) => {
                        return prefer_store_error(
                            &session,
                            Err(EngineFailure::integrity(format!(
                                "restore: RestoreApplications.plist source is invalid: {error}"
                            ))),
                        );
                    }
                    Err(RestoreApplicationsError::Device(error)) => {
                        return Err(EngineFailure::new(
                            ErrorKind::DeviceUnavailable,
                            format!("restore: RestoreApplications.plist could not be staged: {error}"),
                        ));
                    }
                }
            }
        }
        if let Some(error) = session.error() {
            return Err(EngineFailure::from(error));
        }
        let connect = mobilebackup2::connect(provider, udid);
        let stage = "mobilebackup2 connect";
        let mut mb2 = run_stage(&cancel, label, stage, timeouts::CONNECT, connect)
            .await?
            .map_err(|error| {
                EngineFailure::from_idevice(&format!("{label} mobilebackup2 connect failed"), error)
            })?;
        // netmuxd owns the device heartbeat, so no heartbeat service is opened here.
        let observer = mobilebackup2::spawn_cancel_observer(provider, cancel.clone());
        let observer = match cancel_or_timeout(&cancel, timeouts::PROBE, observer).await {
            // The biased select below reports the cancellation.
            Err(Interrupt::Cancelled) => None,
            setup => setup
                .unwrap_or(Err(IdeviceError::Timeout))
                .inspect_err(|error| {
                    tracing::warn!(udid = %udid, %error, "mb2: proceeding without device cancel observer")
                })
                .ok(),
        };
        let delegate = BackupStorage::new(session.clone(), sandbox, progress);
        let res = tokio::select! {
            biased;
            _ = cancel.cancelled() => Err(EngineFailure::new(
                ErrorKind::Cancelled,
                format!("{label} cancelled"),
            )),
            r = async {
                match spec {
                    TransferSpec::Backup { .. } => {
                        mb2.backup_from_path(root, Some(source), None, &delegate).await
                    }
                    TransferSpec::Restore { options, .. } => {
                        mb2.restore_from_path(root, Some(source), Some(options), &delegate).await
                    }
                }
            } => r.map_err(|error| EngineFailure::from_active_transfer(
                &format!("{label} mobilebackup2 transfer failed"),
                error,
            )),
            // Off charger iOS standby reaps service sockets ~15 min in; hold
            // the keep-awake assertion Finder's Wi-Fi sync uses.
            _ = crate::power_assertion::keep_device_awake(provider, udid, label) => unreachable!(),
        };
        if let Some(observer) = observer {
            observer.abort();
        }
        let fallback = format!("the device reported a {label} error");
        let transfer = prefer_store_error(
            &session,
            match delegate.violation() {
                Some(violation) => Err(EngineFailure::integrity(violation)),
                None => res.and_then(|outcome| {
                    mobilebackup2::verdict(outcome, &fallback).map_err(|error| {
                        mobilebackup2::transfer_verdict_failure(error, is_backup)
                    })
                }),
            },
        );
        let cleanup = mobilebackup2::disconnect_bounded(&mut mb2, label).await;
        // Late cancellation is caught by run_mb2 before it seals the staging manifest.
        merge_transfer_cleanup(transfer, cleanup, &format!("{label}: transport teardown"))
    }
    .await;
    if let Err(error) = &result {
        tracing::debug!(udid = %udid, error = %error.detail, "mb2: {label} failed");
    }
    if restore_apps_staged && result.is_err() {
        remove_restore_applications(provider, udid).await;
    }
    result.map(|()| session)
}

/// Registers the transfer under its job id (Busy while one runs for this
/// device), then runs it on a dedicated current-thread runtime:
/// BackupDelegate's object I/O is synchronous, so a slow NAS must not starve
/// the shared runtime's workers.
fn run_transfer_export(
    engine: &AvEngine,
    udid: String,
    source: String,
    job_id: String,
    spec: TransferSpec,
    progress: ProgressSink,
) -> Result<u64, EngineFailure> {
    let span = operation_span(&job_id, spec.label(), &udid);
    let lease = engine
        .operations
        .register_transfer(job_id, udid.clone())
        .map_err(|error| {
            EngineFailure::new(
                ErrorKind::Busy,
                match error {
                    RegisterError::DeviceBusy => {
                        "a backup or restore is already running for this device"
                    }
                    RegisterError::DuplicateOperation => "this operation id is already running",
                },
            )
        })?;
    let cancel = lease.cancellation_token();
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .expect("transfer runtime");
    let result = runtime.block_on(
        run_mb2(
            engine.context(),
            &udid,
            &source,
            spec,
            progress,
            cancel.clone(),
        )
        .instrument(span),
    );
    result.map_err(|mut failure| {
        if cancel.is_cancelled() {
            failure.kind = ErrorKind::Cancelled;
        }
        failure
    })
}

/// Runs a backup into a staging whole-file object manifest. An empty base ID
/// starts the first full snapshot. On rc 0, `added_bytes` (if non-null)
/// receives the payload bytes this backup added to the object pool.
#[no_mangle]
pub extern "C" fn av_snapshot_build(
    engine: *mut AvEngine,
    udid: *const c_char,
    job_id: *const c_char,
    snapshot_id: *const c_char,
    base_snapshot_id: *const c_char,
    cb: BackupCb,
    callback_id: usize,
    added_bytes: *mut u64,
    error: *mut AvError,
) -> i32 {
    if !added_bytes.is_null() {
        unsafe { *added_bytes = 0 };
    }
    guard_error(error, || {
        let (engine, udid) = unsafe { engine_udid(engine, udid) }?;
        let job_id = unsafe { req_str(job_id, "bad job id") }?;
        let snapshot_id = unsafe { req_str(snapshot_id, "bad snapshot id") }?;
        let base_snapshot_id = unsafe { in_str(base_snapshot_id) }
            .filter(|value| !value.is_empty())
            .map(str::to_owned);
        let spec = TransferSpec::Backup {
            snapshot_id,
            base_snapshot_id,
        };
        let progress = ProgressSink {
            callback: cb,
            id: callback_id,
        };
        let added = run_transfer_export(engine, udid.clone(), udid, job_id, spec, progress)?;
        if !added_bytes.is_null() {
            unsafe { *added_bytes = added };
        }
        Ok(())
    })
}

/// Restores one immutable whole-file object snapshot.
#[no_mangle]
pub extern "C" fn av_snapshot_restore(
    engine: *mut AvEngine,
    udid: *const c_char,
    job_id: *const c_char,
    source: *const c_char,
    snapshot_id: *const c_char,
    password: *const c_char,
    system_files: i32,
    reboot: i32,
    settings_from_backup: i32,
    remove_items_not_restored: i32,
    cb: BackupCb,
    callback_id: usize,
    error: *mut AvError,
) -> i32 {
    guard_error(error, || {
        let (engine, udid) = unsafe { engine_udid(engine, udid) }?;
        let job_id = unsafe { req_str(job_id, "bad job id") }?;
        let source = unsafe { req_str(source, "bad backup source") }?;
        let snapshot_id = unsafe { req_str(snapshot_id, "bad backup snapshot id") }?;
        // Never copy the backup first: it would temporarily double hundreds of
        // gigabytes on the NAS, and the sealed CAS store already protects history.
        let mut options = RestoreOptions::new()
            .with_copy(false)
            .with_preserve_settings(settings_from_backup == 0)
            .with_system_files(system_files != 0)
            .with_reboot(reboot != 0)
            .with_remove_items_not_restored(remove_items_not_restored != 0);
        if let Some(password) = unsafe { in_str(password) }.filter(|s| !s.is_empty()) {
            options = options.with_password(password);
        }
        let spec = TransferSpec::Restore {
            snapshot_id,
            options,
        };
        let progress = ProgressSink {
            callback: cb,
            id: callback_id,
        };
        run_transfer_export(engine, udid, source, job_id, spec, progress).map(|_| ())
    })
}

#[cfg(test)]
mod tests {
    use super::{merge_transfer_cleanup, EngineFailure, ErrorKind};

    #[test]
    fn cleanup_failure_does_not_replace_primary_kind() {
        let primary = EngineFailure::new(ErrorKind::Protocol, "connection interrupted");
        let error = merge_transfer_cleanup::<()>(
            Err(primary),
            Err("sync unlock failed: broken pipe".into()),
            "sync session teardown",
        )
        .unwrap_err();

        assert_eq!(error.kind, ErrorKind::Protocol);
        assert!(error.detail.contains("connection interrupted"));
        assert!(error.detail.contains("sync unlock failed: broken pipe"));
    }
}
