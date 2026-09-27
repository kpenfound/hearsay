package version

import "testing"

func TestInfoNeverReportsEmptyFields(t *testing.T) {
	got := Info()
	if got.Version == "" || got.Commit == "" || got.Date == "" {
		t.Errorf("Info() = %+v, want every field populated", got)
	}
}
