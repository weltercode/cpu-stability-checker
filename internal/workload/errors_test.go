package workload

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestExternalFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spec  ExternalSpec
		setup bool
		fault bool
	}{
		{"missing executable timer", ExternalSpec{Command: filepath.Join(t.TempDir(), "missing.exe")}, true, false},
		{"missing executable piped", ExternalSpec{Command: filepath.Join(t.TempDir(), "missing.exe"), Beat: "stdout"}, true, false},
		{"invalid working directory", func() ExternalSpec { s := helperSpec(nil); s.Dir = filepath.Join(t.TempDir(), "missing"); return s }(), true, false},
		{"unexpected exit", helperSpec(map[string]string{"FAKE_EXIT": "7"}), true, false},
		{"declared fault exit", func() ExternalSpec {
			s := helperSpec(map[string]string{"FAKE_EXIT": "7"})
			s.FaultExit = []int{7}
			return s
		}(), false, true},
		{"fault output", func() ExternalSpec {
			s := helperSpec(map[string]string{"FAKE_STATUS": "hardware failure"})
			s.FaultPattern = "hardware failure"
			return s
		}(), false, true},
		{"success", helperSpec(nil), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, err := tc.spec.run(ctx, nopReporter{})
			if IsSetupError(err) != tc.setup || (err != nil) != (tc.setup || tc.fault) {
				t.Fatalf("err=%v setup=%v", err, IsSetupError(err))
			}
		})
	}
}

func TestWrappedSetupErrorRetainsCause(t *testing.T) {
	cause := errors.New("cannot create configuration")
	err := fmt.Errorf("workload failed: %w", NewSetupError(cause))
	if !IsSetupError(err) || !errors.Is(err, cause) {
		t.Fatalf("setup classification/cause lost: %v", err)
	}
}

func TestUnknownWorkloadIsASetupError(t *testing.T) {
	_, err := RunOne(context.Background(), "does-not-exist-review", nopReporter{})
	if !IsSetupError(err) {
		t.Fatalf("unknown workload did not return a setup error: %v", err)
	}
}
