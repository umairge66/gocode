package git

import (
	"testing"
)

func TestParseBranch(t *testing.T) {
	if b := ParseBranch(0, []byte("main\n")); b.Detached || b.Name != "main" {
		t.Fatalf("on-branch parse = %+v", b)
	}
	if b := ParseBranch(1, []byte("")); !b.Detached {
		t.Fatalf("detached (exit 1) parse = %+v", b)
	}
	if b := ParseBranch(0, []byte("  \n")); !b.Detached {
		t.Fatalf("blank output should be detached, got %+v", b)
	}
}
