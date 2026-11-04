package codebase

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateUTF8NeverSplitsARune(t *testing.T) {
	// ASCII is untouched, exactly as before.
	if got := truncateUTF8(strings.Repeat("a", 100), 60); got != strings.Repeat("a", 60) {
		t.Errorf("ascii: %q", got)
	}
	if got := truncateUTF8("short", 60); got != "short" {
		t.Errorf("short string changed: %q", got)
	}
	// Every cut point of mixed 1-, 2-, 3- and 4-byte characters stays valid.
	s := strings.Repeat("aé食😀", 30)
	for max := 0; max <= len(s); max++ {
		got := truncateUTF8(s, max)
		if !utf8.ValidString(got) {
			t.Fatalf("max=%d produced invalid UTF-8: %q", max, got)
		}
		if len(got) > max {
			t.Fatalf("max=%d returned %d bytes", max, len(got))
		}
		if max-len(got) >= utf8.UTFMax {
			t.Fatalf("max=%d backed off too far (%d bytes)", max, len(got))
		}
	}
}
