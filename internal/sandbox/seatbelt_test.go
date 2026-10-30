package sandbox

import (
	"strings"
	"testing"
)

func TestSeatbeltProfileHeader(t *testing.T) {
	p := Profile{Network: NetworkPolicy{Restricted: false}, WriteUnrestricted: true, LogTag: "lt"}
	prof := SeatbeltProfile(p)
	if !strings.HasPrefix(prof, "(version 1)\n") {
		t.Errorf("profile must start with (version 1); got %q", prof[:20])
	}
	if !strings.Contains(prof, `(deny default (with message "lt"))`) {
		t.Error("missing deny-default clause with log tag")
	}
	if !strings.Contains(prof, "(allow process-exec)") {
		t.Error("missing essential process-exec clause")
	}
}

func TestSeatbeltFileReadClauses(t *testing.T) {
	p := Profile{
		ReadDeny:          []string{"/home/user/.ssh", "/etc/shadow"},
		ReadAllow:         []string{"/home/user/.ssh/known_hosts"},
		WriteUnrestricted: true,
		LogTag:            "lt",
	}
	prof := SeatbeltProfile(p)

	if !strings.Contains(prof, "(allow file-read*)") {
		t.Error("read baseline (allow file-read*) missing")
	}
	// deny block over the deny roots
	if !strings.Contains(prof, "(deny file-read*") {
		t.Error("(deny file-read* ...) block missing")
	}
	for _, want := range []string{
		`(subpath "/home/user/.ssh")`,
		`(subpath "/etc/shadow")`,
	} {
		if !strings.Contains(prof, want) {
			t.Errorf("deny read entry %s missing", want)
		}
	}
	// re-allow carve-out
	if !strings.Contains(prof, "(allow file-read*") ||
		!strings.Contains(prof, `(subpath "/home/user/.ssh/known_hosts")`) {
		t.Error("read carve-out (allow file-read* known_hosts) missing")
	}
	if !strings.Contains(prof, "(allow file-read-metadata") {
		t.Error("directory metadata allow missing when denies present")
	}
}

func TestSeatbeltFileWriteClauses(t *testing.T) {
	p := Profile{
		WriteAllow: []string{"/home/user/project", "/tmp"},
		WriteDeny:  []string{"/home/user/project/.git"},
		LogTag:     "lt",
	}
	prof := SeatbeltProfile(p)

	if !strings.Contains(prof, "(allow file-write*") {
		t.Error("(allow file-write* ...) block missing")
	}
	for _, want := range []string{
		`(subpath "/home/user/project")`,
		`(subpath "/tmp")`,
	} {
		if !strings.Contains(prof, want) {
			t.Errorf("write allow entry %s missing", want)
		}
	}
	if !strings.Contains(prof, "(deny file-write*") ||
		!strings.Contains(prof, `(subpath "/home/user/project/.git")`) {
		t.Error("write deny carve-out missing")
	}
	// A restricted write profile must NOT blanket-allow writes.
	if strings.Contains(prof, "(allow file-write*)") {
		t.Error("restricted write profile must not emit blanket (allow file-write*)")
	}
}

func TestSeatbeltWriteUnrestricted(t *testing.T) {
	p := Profile{WriteUnrestricted: true, LogTag: "lt"}
	prof := SeatbeltProfile(p)
	if !strings.Contains(prof, "(allow file-write*)") {
		t.Error("unrestricted write must emit blanket (allow file-write*)")
	}
}

func TestSeatbeltNetwork(t *testing.T) {
	// Unrestricted network.
	open := SeatbeltProfile(Profile{Network: NetworkPolicy{Restricted: false}, WriteUnrestricted: true})
	if !strings.Contains(open, "(allow network*)") {
		t.Error("unrestricted network must emit (allow network*)")
	}

	// Restricted with local binding + proxy ports.
	restricted := SeatbeltProfile(Profile{
		Network:           NetworkPolicy{Restricted: true, AllowLocalBinding: true, HTTPProxyPort: 8080, SOCKSProxyPort: 1080},
		WriteUnrestricted: true,
	})
	if strings.Contains(restricted, "(allow network*)") {
		t.Error("restricted network must not emit (allow network*)")
	}
	for _, want := range []string{
		`(allow network-bind (local ip "*:*"))`,
		`(allow network-outbound (remote ip "localhost:8080"))`,
		`(allow network-outbound (remote ip "localhost:1080"))`,
	} {
		if !strings.Contains(restricted, want) {
			t.Errorf("restricted network clause %s missing", want)
		}
	}
}

func TestSeatbeltGlobRendersRegex(t *testing.T) {
	p := Profile{ReadDeny: []string{"/home/*/secrets"}, WriteUnrestricted: true}
	prof := SeatbeltProfile(p)
	if !strings.Contains(prof, `(regex #"^/home/[^/]*/secrets")`) {
		t.Errorf("glob deny should render as anchored regex; got:\n%s", prof)
	}
}

func TestSeatbeltPty(t *testing.T) {
	p := Profile{AllowPty: true, WriteUnrestricted: true}
	prof := SeatbeltProfile(p)
	if !strings.Contains(prof, "(allow pseudo-tty)") || !strings.Contains(prof, `(literal "/dev/ptmx")`) {
		t.Error("pty clauses missing when AllowPty set")
	}
	noPty := SeatbeltProfile(Profile{WriteUnrestricted: true})
	if strings.Contains(noPty, "(allow pseudo-tty)") {
		t.Error("pty clauses present without AllowPty")
	}
}

func TestSeatbeltArgvShape(t *testing.T) {
	p := Profile{
		WriteUnrestricted: true,
		UnsetEnv:          []string{"BAD"},
		SetEnv:            map[string]string{"GOOD": "1"},
		Shell:             "zsh",
	}
	argv := SeatbeltArgv(p, "echo hi", "/bin/zsh")

	if argv[0] != "env" {
		t.Errorf("argv[0] = %q, want env", argv[0])
	}
	// Expect: env -u BAD GOOD=1 /usr/bin/sandbox-exec -p <profile> /bin/zsh -c "echo hi"
	joined := strings.Join(argv, "\x00")
	if !strings.Contains(joined, "env\x00-u\x00BAD\x00GOOD=1\x00"+SeatbeltExecPath+"\x00-p\x00") {
		t.Errorf("argv prefix shape wrong: %v", argv)
	}
	n := len(argv)
	if argv[n-3] != "/bin/zsh" || argv[n-2] != "-c" || argv[n-1] != "echo hi" {
		t.Errorf("argv tail wrong: %v", argv[n-3:])
	}
	// The profile string must be the argument right after -p.
	pIdx := indexOf(argv, "-p")
	if pIdx < 0 || !strings.HasPrefix(argv[pIdx+1], "(version 1)") {
		t.Errorf("profile not inlined after -p: %v", argv)
	}
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}
