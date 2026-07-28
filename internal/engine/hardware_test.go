package engine

import "testing"

func TestFriendlyCarrier(t *testing.T) {
	cases := []struct{ in, want string }{
		{"com.apple.MTS_ru", "MTS (RU)"},
		{"com.apple.MegaFon_ru", "MegaFon (RU)"},
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

func TestSlotLabel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"kOne", "Primary"},
		{"kTwo", "Secondary"},
		{"", ""},
	}
	for _, c := range cases {
		if got := slotLabel(c.in); got != c.want {
			t.Errorf("slotLabel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBatteryHealthMapping(t *testing.T) {
	tests := []struct {
		name       string
		battery    rawBattery
		wantHealth uint64
		wantMax    uint64
	}{
		{
			name: "normalized max capacity is not health",
			battery: rawBattery{
				MaxCapacity:           100,
				NominalChargeCapacity: 3829,
				AppleRawMaxCapacity:   3800,
				DesignCapacity:        4352,
			},
			wantHealth: 88,
			wantMax:    3829,
		},
		{
			name: "raw capacity fallback",
			battery: rawBattery{
				AppleRawMaxCapacity: 1521,
				DesignCapacity:      1550,
			},
			wantHealth: 98,
			wantMax:    1521,
		},
		{
			name: "legacy max capacity in milliamp hours",
			battery: rawBattery{
				MaxCapacity:    1397,
				DesignCapacity: 1430,
			},
			wantHealth: 98,
			wantMax:    1397,
		},
		{
			name: "explicit compact OS percentage wins",
			battery: rawBattery{
				MaximumCapacityPercent: 86,
				NominalChargeCapacity:  3829,
				DesignCapacity:         4352,
			},
			wantHealth: 86,
			wantMax:    3829,
		},
		{
			name: "explicit spaced OS percentage wins",
			battery: rawBattery{
				MaximumCapacityPercent:           86,
				MaximumCapacityPercentWithSpaces: 84,
				NominalChargeCapacity:            3829,
				DesignCapacity:                   4352,
			},
			wantHealth: 84,
			wantMax:    3829,
		},
		{
			name: "replacement battery is capped at one hundred",
			battery: rawBattery{
				NominalChargeCapacity: 4500,
				DesignCapacity:        4352,
			},
			wantHealth: 100,
			wantMax:    4500,
		},
		{
			name: "missing baseline omits health",
			battery: rawBattery{
				NominalChargeCapacity: 3829,
			},
			wantHealth: 0,
			wantMax:    3829,
		},
		{
			name: "negative values are discarded",
			battery: rawBattery{
				MaxCapacity:           -1,
				NominalChargeCapacity: -1,
				AppleRawMaxCapacity:   -1,
				DesignCapacity:        -1,
			},
			wantHealth: 0,
			wantMax:    0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := (rawDeviceInfo{Battery: tt.battery}).toHardwareInfo()
			if got.BatteryHealthPct != tt.wantHealth {
				t.Errorf("BatteryHealthPct = %d, want %d", got.BatteryHealthPct, tt.wantHealth)
			}
			if got.BatteryMaxCapacity != tt.wantMax {
				t.Errorf("BatteryMaxCapacity = %d, want %d", got.BatteryMaxCapacity, tt.wantMax)
			}
		})
	}
}
