package cli

import (
	"io"
	"os"
	"strings"
	"testing"
)

func TestCapLine(t *testing.T) {
	cases := []struct {
		name  string
		probe func() (string, bool)
		want  []string
		never []string
	}{
		{
			"available with detail",
			func() (string, bool) { return "32 logical CPUs", true },
			[]string{"available", "32 logical CPUs"},
			[]string{"unavailable"},
		},
		{
			"unavailable with reason",
			func() (string, bool) { return "no such counter", false },
			[]string{"unavailable", "no such counter"},
			nil,
		},
		{
			"available without detail has no trailing colon",
			func() (string, bool) { return "", true },
			[]string{"available"},
			[]string{": ", "unavailable"},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			old := os.Stdout
			r, w, _ := os.Pipe()
			os.Stdout = w
			capLine("probe name", tt.probe)
			w.Close()
			os.Stdout = old
			gotBytes, err := io.ReadAll(r)
			if err != nil {
				t.Fatalf("read captured output: %v", err)
			}
			got := string(gotBytes)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("capLine output %q, want to contain %q", got, want)
				}
			}
			for _, bad := range tt.never {
				if strings.Contains(got, bad) {
					t.Errorf("capLine output %q, want NOT to contain %q", got, bad)
				}
			}
		})
	}
}

// caps is informational: it answers on every machine (available or not)
// and never fails the caller.
func TestCapsCmdAlwaysSucceeds(t *testing.T) {
	if code := capsCmd(nil); code != 0 {
		t.Fatalf("capsCmd() exit code = %d, want 0", code)
	}
}
