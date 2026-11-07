package wmisensor

import "testing"

func TestCoreClocks(t *testing.T) {
	cases := []struct {
		name string
		in   []Reading
		want map[int]float64
	}{
		{"lhm amd naming", []Reading{
			{Type: "Clock", Name: "Core #1", Value: 5533},
			{Type: "Clock", Name: "Core #16", Value: 4480},
			{Type: "Clock", Name: "Core Total", Value: 4000},  // aggregate, not a core
			{Type: "Temperature", Name: "Core #2", Value: 80}, // wrong type
			{Type: "Load", Name: "Core #1", Value: 100},       // wrong type
		}, map[int]float64{0: 5533, 15: 4480}},
		{"intel effective naming", []Reading{
			{Type: "Clock", Name: "Core #1 Effective Clock", Value: 5100},
		}, map[int]float64{0: 5100}},
		{"none", []Reading{{Type: "Temperature", Name: "CPU (Tctl/Tdie)"}}, map[int]float64{}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := CoreClocks(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("core %d = %v, want %v", k, got[k], v)
				}
			}
		})
	}
}

func TestFind(t *testing.T) {
	in := []Reading{
		{Type: "Temperature", Name: "CPU (Tctl/Tdie)", Value: 72},
		{Type: "Power", Name: "CPU Package", Value: 118},
		{Type: "Voltage", Name: "CPU Core", Value: 1.21},
	}
	if r, ok := Find(in, "Temperature", "tctl"); !ok || r.Value != 72 {
		t.Errorf("temp lookup: %+v %v", r, ok)
	}
	if r, ok := Find(in, "Power", "cpu package"); !ok || r.Value != 118 {
		t.Errorf("power lookup: %+v %v", r, ok)
	}
	if _, ok := Find(in, "Fan", "cpu"); ok {
		t.Error("type filter must exclude other types")
	}
}
