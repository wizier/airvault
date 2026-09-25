//! Wait budgets shared by several modules, named by *who* is being waited for.
//! A budget only one module uses lives next to that user.

use std::time::Duration;

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

/// How often a watcher re-reads state it cannot subscribe to.
pub(crate) const POLL_INTERVAL: Duration = Duration::from_secs(2);
