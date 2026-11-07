package temperature

import (
	"fmt"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/hwinfo"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
	"github.com/weltercode/cpu-stability-checker/internal/wmisensor"
)

func defaultReaders() []Reader { return []Reader{&hwReader{}, &wmiReader{}, &systemReader{}} }

var cpuTemperatureNames = []string{"tctl", "cpu package", "tdie", "cpu die", "cpu core"}

type hwReader struct{ source *hwinfo.Source }

func (r *hwReader) Read() (Reading, error) {
	if r.source == nil {
		s, err := hwinfo.Open()
		if err != nil {
			return Reading{}, err
		}
		r.source = s
	}
	reads, at := r.source.Snapshot()
	if candidate, ok := hwTemperature(reads, at, time.Now()); ok {
		return candidate, nil
	}
	r.source.Close()
	r.source = nil
	return Reading{}, fmt.Errorf("HWiNFO has no fresh CPU temperature")
}

func hwTemperature(reads []hwinfo.Reading, at, now time.Time) (Reading, bool) {
	for _, name := range cpuTemperatureNames {
		for _, v := range reads {
			if v.Type != hwinfo.TypeTemp || (v.Unit != "°C" && v.Unit != "C") {
				continue
			}
			if found, ok := hwinfo.Find([]hwinfo.Reading{v}, hwinfo.TypeTemp, name); ok {
				candidate := Reading{found.Value, "HWiNFO", found.Name, at, true}
				if fresh(candidate, now, MaxAge) {
					return candidate, true
				}
			}
		}
	}
	return Reading{}, false
}
func (r *hwReader) Close() {
	if r.source != nil {
		r.source.Close()
	}
}

type wmiReader struct{ source *wmisensor.Source }

func (r *wmiReader) Read() (Reading, error) {
	if r.source == nil {
		s, err := wmisensor.Open()
		if err != nil {
			return Reading{}, err
		}
		r.source = s
	}
	reads, at := r.source.Snapshot()
	if candidate, ok := wmiTemperature(reads, at, time.Now(), r.source.Provider()); ok {
		return candidate, nil
	}
	return Reading{}, fmt.Errorf("WMI has no fresh CPU temperature")
}

func wmiTemperature(reads []wmisensor.Reading, at, now time.Time, provider string) (Reading, bool) {
	for _, name := range cpuTemperatureNames {
		for _, v := range reads {
			if found, ok := wmisensor.Find([]wmisensor.Reading{v}, "Temperature", name); ok {
				candidate := Reading{found.Value, "WMI/" + provider, found.Name, at, true}
				if fresh(candidate, now, MaxAge) {
					return candidate, true
				}
			}
		}
	}
	return Reading{}, false
}
func (r *wmiReader) Close() {
	if r.source != nil {
		r.source.Close()
	}
}

type systemReader struct{ source *sysinfo.TemperatureSensor }

func (r *systemReader) Read() (Reading, error) {
	if r.source == nil {
		s, err := sysinfo.NewTemperatureSensor()
		if err != nil {
			return Reading{}, err
		}
		r.source = s
	}
	if value, ok := r.source.Read(); ok {
		return Reading{value, "system", "thermal zone (not CPU)", time.Now(), false}, nil
	}
	return Reading{}, fmt.Errorf("system thermal zone unavailable")
}
func (r *systemReader) Close() {
	if r.source != nil {
		r.source.Close()
	}
}
