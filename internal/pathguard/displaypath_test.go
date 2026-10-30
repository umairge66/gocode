package pathguard

import (
	"strings"
	"testing"
)

func TestDisplayPathSanitizesAndTruncates(t *testing.T) {
	if got := displayPath("/tmp/a\nb\x1b[31mc‮"); got != "/tmp/a�bc��" {
		t.Fatalf("sanitize: %q", got)
	}
	if got := displayPath("/clean/path"); got != "/clean/path" {
		t.Fatalf("clean: %q", got)
	}
	long := "/" + strings.Repeat("x", 199)
	got := displayPath(long)
	if want := long[:160] + "… [+40 chars]"; got != want {
		t.Fatalf("truncate: %q", got)
	}
	// A surrogate pair straddling the cap is not split.
	emoji := strings.Repeat("a", 159) + "😀" + "b"
	if got := displayPath(emoji); got != strings.Repeat("a", 159)+"… [+3 chars]" {
		t.Fatalf("surrogate: %q", got)
	}
}
