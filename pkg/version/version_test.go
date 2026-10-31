package version

import "testing"

func TestGetVersionPrefersStampedValue(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })

	version = "1.2.3"
	if got := GetVersion(); got != "1.2.3" {
		t.Fatalf("GetVersion() = %q, want the stamped 1.2.3", got)
	}
}
