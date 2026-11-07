package human

import "testing"

func TestCount(t *testing.T) {
	tests := []struct {
		in   uint64
		want string
	}{
		{in: 0, want: "0"},
		{in: 999, want: "999"},
		{in: 1_000, want: "1.0k"},
		{in: 12_345, want: "12.3k"},
		{in: 1_000_000, want: "1.00M"},
		{in: 1_500_000_000, want: "1.50B"},
	}

	for _, tt := range tests {
		if got := Count(tt.in); got != tt.want {
			t.Errorf("Count(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestBytes(t *testing.T) {
	tests := []struct {
		in   uint64
		want string
	}{
		{in: 0, want: "0 B"},
		{in: 1023, want: "1023 B"},
		{in: 1024, want: "1.0 KiB"},
		{in: 1536, want: "1.5 KiB"},
		{in: 1 << 20, want: "1.0 MiB"},
		{in: 1 << 30, want: "1.0 GiB"},
	}

	for _, tt := range tests {
		if got := Bytes(tt.in); got != tt.want {
			t.Errorf("Bytes(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestInts(t *testing.T) {
	if got := Ints([]int{0, 2, 3}); got != "0,2,3" {
		t.Fatalf("Ints() = %q, want \"0,2,3\"", got)
	}
	if got := Ints(nil); got != "" {
		t.Fatalf("Ints(nil) = %q, want empty", got)
	}
}

func TestEnabled(t *testing.T) {
	if got := Enabled(true); got != "ENABLED" {
		t.Fatalf("Enabled(true) = %q, want ENABLED", got)
	}
	if got := Enabled(false); got != "DISABLED" {
		t.Fatalf("Enabled(false) = %q, want DISABLED", got)
	}
}
