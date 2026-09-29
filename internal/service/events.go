package service

import (
	"github.com/wizier/airvault/internal/engine"
	"github.com/wizier/airvault/internal/events"
)

// The SSE contract: every event the service publishes and its payload. The
// web mirrors it in lib/events.svelte.ts.

// deviceEvent names the device whose state or catalog changed; a client
// refetches what the event is about.
type deviceEvent struct {
	UDID       string `json:"udid"`
	Connection string `json:"connection,omitempty"`
	LockScreen *bool  `json:"lockScreen,omitempty"`
}

func deviceEventOf(kind, udid string) events.Event {
	return events.Event{Type: kind, Data: deviceEvent{UDID: udid}}
}

func deviceAdded(udid string) events.Event   { return deviceEventOf("device.added", udid) }
func deviceRemoved(udid string) events.Event { return deviceEventOf("device.removed", udid) }
func deviceUpdated(udid string) events.Event { return deviceEventOf("device.updated", udid) }
func deviceOffline(udid string) events.Event { return deviceEventOf("device.offline", udid) }

func deviceOnline(udid, connection string) events.Event {
	return events.Event{Type: "device.online", Data: deviceEvent{UDID: udid, Connection: connection}}
}

func connectionChanged(udid, connection string) events.Event {
	return events.Event{Type: "device.updated", Data: deviceEvent{UDID: udid, Connection: connection}}
}

func lockScreenChanged(udid string, lockScreen bool) events.Event {
	return events.Event{Type: "device.updated", Data: deviceEvent{UDID: udid, LockScreen: &lockScreen}}
}

func backupCatalogChanged(udid string) events.Event { return deviceEventOf("backup.catalog", udid) }
func appCatalogChanged(udid string) events.Event    { return deviceEventOf("app.catalog", udid) }

// pairEvent tells the pairing wizard to reread its state; Paired is set when
// the pairing itself changed.
type pairEvent struct {
	UDID   string `json:"udid"`
	Paired *bool  `json:"paired,omitempty"`
}

func pairableChanged(udid string) events.Event {
	return events.Event{Type: "pair.changed", Data: pairEvent{UDID: udid}}
}

func pairingChanged(udid string, paired bool) events.Event {
	return events.Event{Type: "pair.changed", Data: pairEvent{UDID: udid, Paired: &paired}}
}

type trustEvent struct {
	RunID     string                `json:"runId"`
	UDID      string                `json:"udid"`
	Status    engine.PairingOutcome `json:"status"`
	ErrorCode string                `json:"errorCode,omitempty"`
}

// trustStep is one step of a pairing run; a failed step has an error code.
func trustStep(runID, udid string, status engine.PairingOutcome, errorCode string) events.Event {
	return events.Event{Type: "pair.trust", Data: trustEvent{RunID: runID, UDID: udid, Status: status, ErrorCode: errorCode}}
}

type runStartEvent struct {
	RunID      string `json:"runId"`
	UDID       string `json:"udid"`
	Restore    bool   `json:"restore,omitempty"`
	Verify     bool   `json:"verify,omitempty"`
	SnapshotID string `json:"snapshotId,omitempty"`
}

// runStarted announces a backup, or a restore from snapshotID.
func runStarted(run *runReservation, snapshotID string) events.Event {
	return events.Event{Type: "backup.started", Data: runStartEvent{RunID: run.id, UDID: run.udid,
		Restore: run.kind == runKindRestore, Verify: run.kind == runKindVerify, SnapshotID: snapshotID}}
}

// runProgressed is transient: the next frame supersedes it.
func runProgressed(progress RunProgress) events.Event {
	return events.Event{Type: "backup.progress", Data: progress, Transient: true}
}

// runEndEvent carries only stable codes; the client localizes them.
type runEndEvent struct {
	RunID     string `json:"runId"`
	UDID      string `json:"udid"`
	State     string `json:"state"`
	Restore   bool   `json:"restore,omitempty"`
	Verify    bool   `json:"verify,omitempty"`
	Auto      bool   `json:"auto,omitempty"`
	ErrorCode string `json:"errorCode,omitempty"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
}

var runEndTypes = map[string]string{
	runStateCompleted: "backup.completed",
	runStateCancelled: "backup.cancelled",
	runStateFailed:    "backup.failed",
}

func runEnded(run *runReservation, state, errorCode string, sizeBytes int64) events.Event {
	return events.Event{Type: runEndTypes[state], Data: runEndEvent{RunID: run.id, UDID: run.udid, State: state,
		Restore: run.kind == runKindRestore, Verify: run.kind == runKindVerify, Auto: run.auto,
		ErrorCode: errorCode, SizeBytes: sizeBytes}}
}

func muxerChanged(status MuxerStatus) events.Event {
	return events.Event{Type: "muxer.changed", Data: status}
}
