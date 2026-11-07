package sysinfo

import (
	"math"
	"testing"
)

func TestKelvinToCelsius(t *testing.T) {
	cases := []struct {
		name   string
		kelvin float64
		want   float64
		wantOK bool
	}{
		{"room temperature", 296.15, 23.0, true},
		{"zero Celsius", 273.15, 0.0, true},
		{"hot zone", 358.15, 85.0, true},
		{"absolute zero is garbage", 0, 0, false},
		{"negative is garbage", -10, 0, false},
		{"below any plausible board", 200, 0, false}, // -73°C
		{"above any plausible zone", 800, 0, false},  // 526.85°C
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := kelvinToCelsius(tt.kelvin)
			if ok != tt.wantOK {
				t.Fatalf("kelvinToCelsius(%v) ok = %v, want %v", tt.kelvin, ok, tt.wantOK)
			}
			if ok && math.Abs(got-tt.want) > 0.01 {
				t.Fatalf("kelvinToCelsius(%v) = %.2f, want %.2f", tt.kelvin, got, tt.want)
			}
		})
	}
}

// A closed (or never-opened) sensor must answer "unknown", never a number.
func TestTemperatureSensorClosedReadsUnknown(t *testing.T) {
	s := &TemperatureSensor{}
	s.Close() // closing twice must not panic
	if c, ok := s.Read(); ok || c != 0 {
		t.Fatalf("closed sensor Read() = (%v, %v), want (0, false)", c, ok)
	}
}

func TestThermalZoneScore(t *testing.T) {
	cases := []struct {
		zoneType string
		want     int
	}{
		{"x86_pkg_temp", 3},
		{"cpu_thermal", 2},
		{"coretemp", 2},
		{"acpitz", 1},
		{"bat_adp", 0},
		{"", 0},
	}
	for _, tt := range cases {
		if got := thermalZoneScore(tt.zoneType); got != tt.want {
			t.Errorf("thermalZoneScore(%q) = %d, want %d", tt.zoneType, got, tt.want)
		}
	}
}
