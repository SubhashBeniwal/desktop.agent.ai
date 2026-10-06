package shellenv

import (
	"os"
	"testing"
)

func TestMergeKeepsOrderAndDedupes(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	merge("/opt/x:/usr/bin::/opt/y")
	if got, want := os.Getenv("PATH"), "/usr/bin:/bin:/opt/x:/opt/y"; got != want {
		t.Fatalf("PATH = %q, want %q", got, want)
	}
}
