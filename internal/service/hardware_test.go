package service

import (
	"testing"

	"github.com/wizier/airvault/internal/engine"
)

func TestFriendlyCarrier(t *testing.T) {
	cases := []struct{ in, want string }{
		{"com.apple.MTS_ru", "MTS (RU)"},
		{"com.apple.ATT_US", "ATT (US)"},
		{"com.apple.CarrierDefault", ""},
		{"", ""},
		{"com.apple.Unknown", "Unknown"}, // no locale suffix -> keep the bare name
	}
	for _, c := range cases {
		if got := friendlyCarrier(c.in); got != c.want {
			t.Errorf("friendlyCarrier(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBatteryHealthMapping(t *testing.T) {
	type gauge = engine.BatteryGauge
	tests := []struct {
		name       string
		battery    gauge
		wantHealth uint64
		wantMax    uint64
	}{
		{"normalized max capacity is not health",
			gauge{MaxCapacity: 100, NominalChargeCapacity: 3829, AppleRawMaxCapacity: 3800, DesignCapacity: 4352}, 88, 3829},
		{"raw capacity fallback", gauge{AppleRawMaxCapacity: 1521, DesignCapacity: 1550}, 98, 1521},
		{"legacy max capacity in milliamp hours", gauge{MaxCapacity: 1397, DesignCapacity: 1430}, 98, 1397},
		{"explicit compact OS percentage wins",
			gauge{MaximumCapacityPercent: 86, NominalChargeCapacity: 3829, DesignCapacity: 4352}, 86, 3829},
		{"explicit spaced OS percentage wins", gauge{MaximumCapacityPercent: 86, MaximumCapacityPercentWithSpaces: 84,
			NominalChargeCapacity: 3829, DesignCapacity: 4352}, 84, 3829},
		{"replacement battery is capped at one hundred", gauge{NominalChargeCapacity: 4500, DesignCapacity: 4352}, 100, 4500},
		{"missing baseline omits health", gauge{NominalChargeCapacity: 3829}, 0, 3829},
		{"negative values are discarded",
			gauge{MaxCapacity: -1, NominalChargeCapacity: -1, AppleRawMaxCapacity: -1, DesignCapacity: -1}, 0, 0},
	}
	for _, tt := range tests {
		got := hardwareInfo(engine.HardwareReport{Battery: tt.battery})
		if got.BatteryHealthPct != tt.wantHealth || got.BatteryMaxCapacity != tt.wantMax {
			t.Errorf("%s: health %d max %d, want %d and %d",
				tt.name, got.BatteryHealthPct, got.BatteryMaxCapacity, tt.wantHealth, tt.wantMax)
		}
	}
}
