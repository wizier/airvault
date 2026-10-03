package service

import (
	"encoding/json"
	"testing"

	"github.com/wizier/airvault/internal/engine"
	"github.com/wizier/airvault/internal/events"
)

// The wire form lib/events.svelte.ts reads.
func TestEventWireForm(t *testing.T) {
	restore := &runReservation{id: "run-1", udid: "udid-1", kind: runKindRestore}
	for _, test := range []struct {
		event      events.Event
		kind, data string
	}{
		{deviceAdded("u"), "device.added", `{"udid":"u"}`},
		{deviceOnline("u", "wifi"), "device.online", `{"udid":"u","connection":"wifi"}`},
		{connectionChanged("u", "usb"), "device.updated", `{"udid":"u","connection":"usb"}`},
		{lockScreenChanged("u", false), "device.updated", `{"udid":"u","lockScreen":false}`},
		{backupCatalogChanged("u"), "backup.catalog", `{"udid":"u"}`},
		{pairableChanged("u"), "pair.changed", `{"udid":"u"}`},
		{pairingChanged("u", false), "pair.changed", `{"udid":"u","paired":false}`},
		{trustStep("run-1", "u", engine.TrustPending, ""), "pair.trust", `{"runId":"run-1","udid":"u","status":"trust_pending"}`},
		{runStarted(restore, "snap"), "backup.started", `{"runId":"run-1","udid":"udid-1","kind":"restore","snapshotId":"snap"}`},
		{runEnded(restore, runStateCompleted, "", 42), "backup.completed",
			`{"runId":"run-1","udid":"udid-1","state":"completed","kind":"restore","sizeBytes":42}`},
		{muxerChanged(MuxerStatus{Up: true, USB: 1}), "muxer.changed", `{"up":true,"usb":1,"wifi":0}`},
	} {
		data, err := json.Marshal(test.event.Data)
		if err != nil || test.event.Type != test.kind || string(data) != test.data || test.event.Transient {
			t.Errorf("event = %s %s (transient %v), %v; want %s %s", test.event.Type, data, test.event.Transient, err, test.kind, test.data)
		}
	}
	if !runProgressed(RunProgress{}).Transient {
		t.Error("a progress frame must be transient")
	}
}
