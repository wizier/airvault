package service

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/engine"
)

func TestAutoWait(t *testing.T) {
	now := time.Date(2026, 9, 26, 20, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { at := now.Add(-d); return &at }
	daily := autoBackupEvery(1)
	// failed lists the latest failures, oldest first, as recordAutoBackup keeps them.
	failed := func(ago ...time.Duration) autoHistory {
		var h autoHistory
		for i, d := range ago {
			h.failures[autoBackupLimit-len(ago)+i] = now.Add(-d)
		}
		return h
	}
	tests := []struct {
		name       string
		every      time.Duration
		lastBackup *time.Time
		history    autoHistory
		wantWait   string
		wantAt     time.Time
	}{
		{"first backup is manual", daily, nil, autoHistory{}, autoWaitFirstBackup, time.Time{}},
		{"daily is due 4 h early", daily, ago(20 * time.Hour), autoHistory{}, "", time.Time{}},
		{"daily not yet due", daily, ago(19 * time.Hour), autoHistory{}, autoWaitSchedule, now.Add(time.Hour)},
		{"weekly", autoBackupEvery(7), ago(6 * 24 * time.Hour), autoHistory{}, autoWaitSchedule,
			now.Add(20 * time.Hour)},
		{"pause outlasts the schedule", daily, ago(48 * time.Hour),
			autoHistory{pausedUntil: now.Add(30 * time.Minute)}, autoWaitPaused, now.Add(30 * time.Minute)},
		{"expired pause", daily, ago(48 * time.Hour),
			autoHistory{pausedUntil: now.Add(-time.Minute)}, "", time.Time{}},
		{"two failures stay under the limit", daily, ago(48 * time.Hour),
			failed(5*time.Hour, 3*time.Hour), "", time.Time{}},
		{"third failure holds until the oldest leaves the window", daily, ago(48 * time.Hour),
			failed(5*time.Hour, 3*time.Hour, 2*time.Hour), autoWaitLimit, now.Add(19 * time.Hour)},
		{"the limit is a rolling window", daily, ago(48 * time.Hour),
			failed(30*time.Hour, 3*time.Hour, 2*time.Hour), "", time.Time{}},
	}
	for _, test := range tests {
		wait, at := autoWait(test.every, test.lastBackup, test.history, now)
		if wait != test.wantWait || !at.Equal(test.wantAt) {
			t.Errorf("%s: autoWait = %q %v, want %q %v", test.name, wait, at, test.wantWait, test.wantAt)
		}
	}
}

func TestClockWindow(t *testing.T) {
	moscow, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatal(err)
	}
	evening := &clockWindow{start: 19 * 60, end: 23 * 60, loc: moscow}
	night := &clockWindow{start: 22 * 60, end: 2 * 60, loc: time.UTC}
	at := func(hour, minute int) time.Time { return time.Date(2026, 9, 26, hour, minute, 0, 0, time.UTC) }
	tests := []struct {
		name   string
		window *clockWindow
		t      time.Time
		want   bool
	}{
		{"no window", nil, at(4, 0), true},
		{"read in the window's zone", evening, at(16, 30), true}, // 19:30 MSK
		{"start is inclusive", evening, at(16, 0), true},
		{"end is exclusive", evening, at(20, 0), false},
		{"before the window", evening, at(12, 0), false},
		{"crossing midnight, late", night, at(23, 30), true},
		{"crossing midnight, early", night, at(1, 59), true},
		{"crossing midnight, outside", night, at(2, 0), false},
	}
	for _, test := range tests {
		if got := test.window.contains(test.t); got != test.want {
			t.Errorf("%s: contains = %v, want %v", test.name, got, test.want)
		}
	}
}

// A finished backup updates the trigger's history as hideRun retires it: only
// automatic attempts count toward the limit, an unanswered manual prompt
// pauses the trigger too, other manual failures leave it alone, and a success
// clears everything.
func TestRecordAutoBackup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newTestService()
		finish := func(auto bool, errorCode string) {
			run := registerRun(s)
			run.auto = auto
			var err error
			if errorCode != "" {
				err = errors.New(errorCode)
			}
			s.completeRun(run, runOutcome{errorCode: errorCode}, err)
		}
		expect := func(step string, failures [autoBackupLimit]time.Time, pausedUntil time.Time) {
			got := s.autoHistory["udid-1"]
			for i := range failures {
				if !got.failures[i].Equal(failures[i]) {
					t.Fatalf("%s: failures = %v, want %v", step, got.failures, failures)
				}
			}
			if !got.pausedUntil.Equal(pausedUntil) {
				t.Fatalf("%s: pausedUntil = %v, want %v", step, got.pausedUntil, pausedUntil)
			}
		}
		var none [autoBackupLimit]time.Time

		finish(false, "device_timeout")
		expect("manual failure", none, time.Time{})
		finish(false, errorBackupNotConfirmed)
		expect("unanswered manual prompt", none, time.Now().Add(autoBackupPause))

		var failures [autoBackupLimit + 1]time.Time
		for i := range failures {
			time.Sleep(2 * time.Hour)
			failures[i] = time.Now()
			finish(true, errorBackupNotConfirmed)
		}
		expect("automatic failures", [autoBackupLimit]time.Time(failures[1:]),
			failures[autoBackupLimit].Add(autoBackupPause))

		finish(false, "")
		if _, kept := s.autoHistory["udid-1"]; kept {
			t.Fatal("success left a history behind")
		}
	})
}

// The trigger fires once per unlock that lasts the dwell; a relock, an
// offline transition or shutdown in between cancels it.
func TestAutoBackupTriggerDwell(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &Service{live: newDeviceRuntimeStore(), autoBackupKick: make(chan struct{}, 1)}
		s.live.applyPresence(map[string]string{"phone": "wifi"})
		fired := make(chan string, 4)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			s.runAutoBackupTrigger(ctx, func(_ context.Context, udid string) { fired <- udid })
			close(done)
		}()
		signal := func(lock engine.ScreenLockSignal) {
			if changed, _, _ := s.live.applyScreenLock("phone", lock, time.Now(), screenLockPairWindow); changed {
				s.wakeAutoBackup()
			}
		}
		unlock := func() { signal(engine.ScreenLockChanged) }
		lock := func() {
			signal(engine.ScreenLockComplete)
			time.Sleep(2 * screenLockPairWindow) // past the lock's trailing pulse
		}
		expectFired := func(step string, want int) {
			synctest.Wait()
			if got := len(fired); got != want {
				t.Fatalf("%s: fired %d times, want %d", step, got, want)
			}
			for range want {
				<-fired
			}
		}

		unlock()
		time.Sleep(autoBackupDwell - time.Second)
		expectFired("before the dwell", 0)
		time.Sleep(time.Second)
		expectFired("after the dwell", 1)
		time.Sleep(time.Minute)
		expectFired("the same unlock again", 0)

		lock()
		unlock()
		time.Sleep(autoBackupDwell / 2)
		lock()
		time.Sleep(autoBackupDwell)
		expectFired("a relock within the dwell", 0)

		unlock()
		time.Sleep(autoBackupDwell / 2)
		s.live.applyPresence(nil)
		time.Sleep(autoBackupDwell)
		expectFired("an offline phone", 0)

		s.live.applyPresence(map[string]string{"phone": "wifi"})
		unlock()
		cancel()
		<-done
		time.Sleep(autoBackupDwell)
		expectFired("after shutdown", 0)
	})
}

func TestAutoBackupSettingsValidation(t *testing.T) {
	window := func(start, end, zone string) *AutoBackupWindow {
		return &AutoBackupWindow{Start: start, End: end, TimeZone: zone}
	}
	invalid := []struct {
		settings AutoBackupSettings
		code     string
	}{
		{AutoBackupSettings{Enabled: true, EveryDays: 2}, "invalid_auto_backup_interval"},
		{AutoBackupSettings{EveryDays: 0}, "invalid_auto_backup_interval"},
		{AutoBackupSettings{EveryDays: 1, Window: window("19:00", "19:00", "UTC")}, "invalid_auto_backup_window"},
		{AutoBackupSettings{EveryDays: 1, Window: window("24:00", "02:00", "UTC")}, "invalid_auto_backup_window"},
		{AutoBackupSettings{EveryDays: 1, Window: window("7pm", "11pm", "UTC")}, "invalid_auto_backup_window"},
		{AutoBackupSettings{EveryDays: 1, Window: window("19:00", "23:00", "")}, "invalid_time_zone"},
		{AutoBackupSettings{EveryDays: 1, Window: window("19:00", "23:00", "Mars/Olympus")}, "invalid_time_zone"},
	}
	for _, test := range invalid {
		_, err := autoBackupRow(test.settings)
		var validation *domain.ValidationError
		if !errors.As(err, &validation) || validation.Code != test.code {
			t.Errorf("autoBackupRow(%+v) = %v, want %s", test.settings, err, test.code)
		}
	}

	settings := AutoBackupSettings{Enabled: true, EveryDays: 3, Window: window("22:30", "01:15", "Europe/Moscow")}
	row, err := autoBackupRow(settings)
	if err != nil {
		t.Fatal(err)
	}
	if *row.WindowStart != 22*60+30 || *row.WindowEnd != 75 {
		t.Fatalf("stored window = %d–%d", *row.WindowStart, *row.WindowEnd)
	}
	back := autoBackupSettingsOf(row)
	if back.Enabled != settings.Enabled || back.EveryDays != settings.EveryDays || *back.Window != *settings.Window {
		t.Fatalf("round trip = %+v, want %+v", back, settings)
	}
}
