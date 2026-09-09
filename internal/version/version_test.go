package version

import "testing"

func TestBuildInfoString(t *testing.T) {
	tests := []struct {
		name string
		in   BuildInfo
		want string
	}{
		{
			name: "all fields set",
			in:   BuildInfo{Version: "v0.1.0", Commit: "abc123", Date: "2026-09-08T00:00:00Z"},
			want: "hearsay v0.1.0 (commit abc123, built 2026-09-08T00:00:00Z)",
		},
		{
			name: "empty fields are not hidden",
			in:   BuildInfo{},
			want: "hearsay  (commit , built )",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInfoNeverReportsEmptyFields(t *testing.T) {
	got := Info()
	if got.Version == "" || got.Commit == "" || got.Date == "" {
		t.Errorf("Info() = %+v, want every field populated", got)
	}
}
