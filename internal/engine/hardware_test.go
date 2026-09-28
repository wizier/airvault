package engine

import (
	"context"
	"reflect"
	"testing"

	"github.com/wizier/airvault/internal/ios"
)

func TestHardwareReport(t *testing.T) {
	p := newTestPhone(t)
	p.phone.SetValue("", "SerialNumber", "F2LXXXX")
	p.phone.SetValue("", "CarrierBundleInfoArray", []any{map[string]any{"CFBundleIdentifier": "com.apple.carrier", "Slot": "kOne"}})
	p.phone.SetValue("com.apple.disk_usage", "TotalDataCapacity", uint64(128<<30))
	p.phone.SetValue("com.apple.fmip", "IsAssociated", true)
	p.phone.Handle(ios.DiagnosticsService, xmlService(func(request map[string]any) map[string]any {
		if request["Request"] != "IORegistry" || request["EntryName"] != "AppleSmartBattery" {
			return map[string]any{"Status": "Failure"}
		}
		return map[string]any{"Status": "Success", "Diagnostics": map[string]any{
			"IORegistry": map[string]any{"CycleCount": 312, "Maximum Capacity Percent": 91, "InstantAmperage": -420},
		}}
	}))

	report, err := p.engine.HardwareReport(context.Background(), p.udid)
	if err != nil {
		t.Fatal(err)
	}
	if report.Lockdown.SerialNumber != "F2LXXXX" || report.Lockdown.ProductType != "iPhone17,1" ||
		!reflect.DeepEqual(report.Lockdown.Carriers, []CarrierBundle{{Bundle: "com.apple.carrier", Slot: "kOne"}}) {
		t.Errorf("lockdown = %+v", report.Lockdown)
	}
	if report.DiskUsage.TotalDataCapacity != 128<<30 {
		t.Errorf("disk usage = %+v", report.DiskUsage)
	}
	if report.FindMy == nil || !*report.FindMy {
		t.Errorf("find my = %v", report.FindMy)
	}
	if want := (BatteryGauge{CycleCount: 312, MaximumCapacityPercentWithSpaces: 91, InstantAmperage: -420}); report.Battery != want {
		t.Errorf("battery = %+v, want %+v", report.Battery, want)
	}
}

// Only the session is required: parts the phone does not provide stay zero.
func TestHardwareReportIsPartial(t *testing.T) {
	p := newTestPhone(t)
	report, err := p.engine.HardwareReport(context.Background(), p.udid)
	if err != nil {
		t.Fatal(err)
	}
	if report.FindMy != nil || report.Battery != (BatteryGauge{}) || report.Lockdown.ProductType != "iPhone17,1" {
		t.Fatalf("partial report = %+v", report)
	}
}
