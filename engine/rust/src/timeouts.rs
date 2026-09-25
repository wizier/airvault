//! Every wait budget in one place, named by *who* is being waited for.
//!
//! Pick a class by the answer to "what has to happen before this returns?" —
//! a person tapping the phone, a socket handshake, the phone doing real work.
//! The mechanics live in `bounded`; only the numbers live here.

use std::time::Duration;

/// A person at the phone: the passcode prompt iOS raises for backup access or
/// an encryption change. Long by design — iOS never expires the prompt itself,
/// so this budget alone decides how much time the user gets.
pub(crate) const PROMPT: Duration = Duration::from_secs(180);

/// Transport handshake with no device-side prompt: usbmux, lockdown connect,
/// StartService, a service the phone answers without asking anyone.
pub(crate) const CONNECT: Duration = Duration::from_secs(20);

/// One small read whose failure never blocks the outcome: a lockdown value, a
/// best-effort gauge. Kept short so a wedged link degrades instead of hanging.
pub(crate) const PROBE: Duration = Duration::from_secs(5);

/// A synchronous call made on behalf of the browser: someone is watching a
/// spinner, so the ceiling is a UI decision, not a protocol one.
pub(crate) const UI_CALL: Duration = Duration::from_secs(15);

/// The phone doing real work: app census, AFC transfers, activation steps.
pub(crate) const DEVICE_WORK: Duration = Duration::from_secs(60);

/// Best-effort teardown. A verdict already exists; cleanup must not delay it.
pub(crate) const TEARDOWN: Duration = Duration::from_secs(2);

// Outliers. Each keeps its own name and the reason no class above fits.

/// Installing streams the whole .ipa and then waits on installd.
pub(crate) const APP_INSTALL: Duration = Duration::from_secs(300);

/// Discovery answers the device list: probe every phone and fail fast, because
/// one unreachable device must not stall the refresh worker.
pub(crate) const DISCOVERY: Duration = Duration::from_secs(4);

/// A muxer conversation (connect + query). Not `PROBE`: its failure decides the
/// outcome rather than degrading it, but a usbmuxd socket that accepts and then
/// hangs must never block the daemon's single refresh worker.
pub(crate) const MUX: Duration = Duration::from_secs(5);

/// Another host (Finder, iTunes) may hold the AFC sync lock; wait briefly,
/// then report the conflict rather than queueing behind it.
pub(crate) const SYNC_LOCK_WAIT: Duration = Duration::from_secs(10);

/// Retry cadence while polling for that lock.
pub(crate) const SYNC_LOCK_RETRY: Duration = Duration::from_millis(200);

/// Pairing covers certificate generation, which is slow in debug shim builds.
/// Not a prompt budget: an unanswered trust dialog returns `trust_pending`.
pub(crate) const PAIRING_ADVANCE: Duration = Duration::from_secs(45);

/// Sleep assertions are renewed well inside the window iOS grants, and retried
/// on this cadence when a renewal fails mid-transfer.
pub(crate) const ASSERTION_RENEW: Duration = Duration::from_secs(600);
pub(crate) const ASSERTION_RETRY: Duration = Duration::from_secs(60);

/// How often a streaming loop wakes to notice cancellation or a closed peer.
pub(crate) const STREAM_TICK: Duration = Duration::from_secs(1);

/// How often a watcher re-reads state it cannot subscribe to.
pub(crate) const POLL_INTERVAL: Duration = Duration::from_secs(2);
