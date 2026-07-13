package version

import "testing"

func TestStringIncludesBuildMetadata(t *testing.T) {
	if got := String(); got != "xira 0.5.0 commit=unknown date=unknown" {
		t.Fatalf("String() = %q", got)
	}
}
