package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/wizier/airvault/internal/events"
	"github.com/wizier/airvault/internal/model"
)

// Only an automatic check that did not finish pauses, and any finished one clears
// the pause.
func TestRecordAutoVerify(t *testing.T) {
	key := autoKey{"phone", runKindVerify}
	tests := []struct {
		name   string
		auto   bool
		state  runState
		paused bool
		want   bool
	}{
		{"an automatic check that failed waits", true, runStateFailed, false, true},
		{"an automatic check that was cancelled waits", true, runStateCancelled, false, true},
		{"a manual check that failed does not", false, runStateFailed, false, false},
		{"a manual check that was cancelled does not", false, runStateCancelled, false, false},
		{"a manual check that failed keeps a pause", false, runStateFailed, true, true},
		{"a completed automatic check clears the pause", true, runStateCompleted, true, false},
		{"a completed manual check clears the pause", false, runStateCompleted, true, false},
	}
	for _, test := range tests {
		registry := newRunRegistry(events.New())
		if test.paused {
			registry.autoHistory[key] = autoHistory{pausedUntil: time.Now().Add(time.Hour)}
		}
		registry.hide(&runReservation{udid: "phone", kind: runKindVerify, auto: test.auto}, test.state, "")
		if _, paused := registry.autoHistory[key]; paused != test.want {
			t.Errorf("%s: paused = %v, want %v", test.name, paused, test.want)
		}
	}
}

// newCheckableService has real runs over a real catalog; addPhone registers a
// phone that has one backup and the given check settings.
func newCheckableService(t *testing.T) (*Service, func(udid string, settings model.Schedule)) {
	t.Helper()
	svc, _ := newStoredService(t)
	svc.app = context.Background()
	svc.runs = newRunRegistry(svc.bus)
	backup := 0
	return svc, func(udid string, settings model.Schedule) {
		t.Helper()
		ctx := context.Background()
		if err := svc.store.Device.Upsert(ctx, &model.Device{UDID: udid, Name: udid, Paired: true}); err != nil {
			t.Fatal(err)
		}
		if err := svc.store.Device.SetAutomaticTasks(ctx, udid, model.AutomaticTasks{AutoVerify: settings}); err != nil {
			t.Fatal(err)
		}
		backup++
		row := model.Backup{ID: fmt.Sprintf("eeeeeeee-0000-4000-8000-%012d", backup), SourceUDID: udid, CreatedAt: 1_700_000_000}
		if err := svc.store.Backup.InsertSnapshot(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
}

func latestChecks(t *testing.T, svc *Service) map[string]*int64 {
	t.Helper()
	summary, err := svc.library.Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string]*int64{}
	for source, row := range summary {
		checks[source] = row.LatestVerified
	}
	return checks
}

func TestStartDueVerify(t *testing.T) {
	const phone = "testphoneudid0007"
	clock := func(hour, minute int) *int64 { return new(int64(hour*60 + minute)) }
	night := model.Schedule{Enabled: true, Days: 7, WindowStart: clock(2, 0), WindowEnd: clock(6, 0)}
	inWindow := time.Date(2026, 9, 26, 3, 0, 0, 0, time.Local)
	tests := []struct {
		name     string
		settings model.Schedule
		now      time.Time
		want     bool
	}{
		{"due inside the window", night, inWindow, true},
		{"outside the window", night, inWindow.Add(12 * time.Hour), false},
		{"turned off", model.Schedule{Days: 7, WindowStart: clock(2, 0), WindowEnd: clock(6, 0)}, inWindow, false},
		{"no window means any time", model.Schedule{Enabled: true, Days: 7}, inWindow.Add(12 * time.Hour), true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			svc, addPhone := newCheckableService(t)
			addPhone(phone, test.settings)
			if err := svc.startDueVerify(t.Context(), test.now); err != nil {
				t.Fatal(err)
			}
			svc.wg.Wait()
			if checked := latestChecks(t, svc)[phone] != nil; checked != test.want {
				t.Fatalf("checked = %v, want %v", checked, test.want)
			}
		})
	}
}

// One pass starts every phone that is due, and a phone just checked is not due again.
func TestStartDueVerifyStartsEveryDuePhone(t *testing.T) {
	const first, second = "testphoneudid0007", "testphoneudid0008"
	svc, addPhone := newCheckableService(t)
	settings := model.Schedule{Enabled: true, Days: 7}
	addPhone(first, settings)
	addPhone(second, settings)
	pass := func() {
		t.Helper()
		if err := svc.startDueVerify(t.Context(), time.Now()); err != nil {
			t.Fatal(err)
		}
		svc.wg.Wait()
	}

	pass()
	checks := latestChecks(t, svc)
	if checks[first] == nil || checks[second] == nil {
		t.Fatalf("one pass left %v, want both checked", checks)
	}
	// Another check would move this mark forward.
	earlier := *checks[first] - 100
	if err := svc.store.Backup.MarkVerified(t.Context(), first, earlier); err != nil {
		t.Fatal(err)
	}
	pass()
	if got := *latestChecks(t, svc)[first]; got != earlier {
		t.Fatalf("a phone just checked ran again: last check = %d", got)
	}
}
