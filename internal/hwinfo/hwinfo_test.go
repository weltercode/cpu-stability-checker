package hwinfo

import (
	"math"
	"testing"
)

func TestCoreClocks(t *testing.T) {
	cases := []struct {
		name string
		in   []Reading
		want map[int]float64
	}{
		{"amd conventional clocks ignore effective threads", []Reading{
			{Type: TypeClock, Name: "Core 0 T0 Effective Clock", Value: 5533},
			{Type: TypeClock, Name: "Core 0 T1 Effective Clock", Value: 57},
			{Type: TypeClock, Name: "Core 15 T1 Effective Clock", Value: 4480},
			{Type: TypeClock, Name: "Core 0 Clock (perf #4)", Value: 5485},
			{Type: TypeClock, Name: "Core 15 Clock (perf #15)", Value: 5300},
			{Type: TypeClock, Name: "Average Effective Clock", Value: 2988},
			{Type: TypeClock, Name: "Bus Clock", Value: 99.8},
			{Type: TypeTemp, Name: "Core 0 T0 Effective Clock", Value: 80}, // wrong type
		}, map[int]float64{0: 5485, 15: 5300}},
		{"amd older one-based", []Reading{
			{Type: TypeClock, Name: "Core #1 Clock", Value: 4600},
			{Type: TypeClock, Name: "Core #16 Clock", Value: 4400},
		}, map[int]float64{0: 4600, 15: 4400}},
		{"intel", []Reading{
			{Type: TypeClock, Name: "P-core 0 Clock", Value: 5100},
			{Type: TypeClock, Name: "P-core 7 Clock", Value: 4900},
		}, map[int]float64{0: 5100, 7: 4900}},
		{"plain names and invalid values", []Reading{
			{Type: TypeClock, Name: " Core 2 Clock ", Unit: "MHz", Value: 5100},
			{Type: TypeClock, Name: "Core 0 Clock", Value: math.NaN()},
			{Type: TypeClock, Name: "Core 1 Clock", Value: math.Inf(1)},
			{Type: TypeClock, Name: "Core 3 Clock", Value: 0},
			{Type: TypeClock, Name: "Core 4 Clock", Value: -1},
			{Type: TypeTemp, Name: "Core 5 Clock", Value: 70},
			{Type: TypeClock, Name: "Core #0 Clock", Value: 5000},
			{Type: TypeClock, Name: "Core 6 Clock", Unit: "GHz", Value: 5},
		}, map[int]float64{2: 5100}},
		{"effective alone never substitutes for Core Clock", []Reading{
			{Type: TypeClock, Name: "Core 0 T0 Effective Clock", Value: 57},
			{Type: TypeClock, Name: "Core #1 Effective Clock", Value: 5000},
			{Type: TypeClock, Name: "Core 1 Effective Clock", Value: 5000},
		}, map[int]float64{}},
		{"none", []Reading{{Type: TypeTemp, Name: "CPU (Tctl/Tdie)"}}, map[int]float64{}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := CoreClocks(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("core %d = %v, want %v (full: %v)", k, got[k], v, got)
				}
			}
		})
	}
}

func TestFind(t *testing.T) {
	in := []Reading{
		{Type: TypeTemp, Name: "CPU (Tctl/Tdie)", Value: 72},
		{Type: TypeTemp, Name: "Motherboard", Value: 40},
		{Type: TypePower, Name: "CPU Package", Value: 118},
		{Type: TypeVolt, Name: "Core #1 VID", Value: 1.206},
	}
	if r, ok := Find(in, TypeTemp, "tdie", "tctl"); !ok || r.Value != 72 {
		t.Errorf("temp lookup: %v %v", r, ok)
	}
	if r, ok := Find(in, TypePower, "cpu package"); !ok || r.Value != 118 {
		t.Errorf("power lookup: %v %v", r, ok)
	}
	if r, ok := Find(in, TypeVolt, "cpu core vid", "vid"); !ok || r.Value != 1.206 {
		t.Errorf("volt lookup falls back to core vid: %v %v", r, ok)
	}
	if _, ok := Find(in, TypeFan, "cpu"); ok {
		t.Error("type filter must exclude other types")
	}
	if _, ok := Find(in, TypeTemp, "nonexistent"); ok {
		t.Error("missing name must not match")
	}
}
