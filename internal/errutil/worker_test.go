package errutil

import (
	"errors"
	"strings"
	"testing"
)

func TestWorkerErrorIs(t *testing.T) {
	e := NewWorkerError(WorkerERespawning, "worker w1 — restarting it; retry reply")
	if !errors.Is(e, &WorkerError{Code: WorkerERespawning}) {
		t.Fatal("expected Is to match on code")
	}
	if errors.Is(e, &WorkerError{Code: WorkerETimeout}) {
		t.Fatal("did not expect match on different code")
	}
	if !strings.Contains(e.Error(), "ERESPAWNING") {
		t.Fatalf("error string missing code: %q", e.Error())
	}
}

func TestNotOwnedError(t *testing.T) {
	e := &NotOwnedError{Path: "/run/sock", UID: 1000}
	if e.Code() != CodeNotOwned {
		t.Fatalf("code = %q", e.Code())
	}
	want := "refusing to bind: /run/sock is owned by uid 1000"
	if e.Error() != want {
		t.Fatalf("message = %q want %q", e.Error(), want)
	}
}
