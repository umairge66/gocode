package inbox

import (
	"strings"
	"testing"
	"time"
)

func TestUDSAddressRoundTrip(t *testing.T) {
	p := "/tmp/cc-socks-1000/12 34é.sock"
	a := UDSAddress(p)
	if a != "uds:/tmp/cc-socks-1000/12%2034%C3%A9.sock" {
		t.Fatalf("UDSAddress = %q", a)
	}
	if !IsPeerAddress(a) {
		t.Fatalf("not a peer address: %q", a)
	}
	if got := ParseAddress(a); got.Scheme != SchemeUDS || got.Target != p {
		t.Fatalf("ParseAddress = %+v", got)
	}
	if got := ParseAddress("uds:%E0%A4"); got.Target != "%E0%A4" {
		t.Fatalf("malformed escape should fall back to raw, got %q", got.Target)
	}
	if got := ParseAddress(`\\.\pipe\cc-msg-abc`); got.Scheme != SchemeUDS {
		t.Fatalf("pipe path scheme = %s", got.Scheme)
	}
}

func TestValidateAddress(t *testing.T) {
	cases := map[string]string{
		"":                      "to must not be empty",
		"uds:":                  "address target must not be empty",
		"uds://host/share/x":    "'uds://host/share/x' is not a local socket address. Use an address from ListAgents.",
		`\\server\pipe\x`:       `'\\server\pipe\x' is not a local socket address. Use an address from ListAgents.`,
		"uds:/tmp/a.sock":       "",
		`\\.\pipe\cc-msg-0123`:  "",
		`\\.\pipe\a\b`:          `'\\.\pipe\a\b' is not a local socket address. Use an address from ListAgents.`,
		`\\.\pipe\trailingdot.`: `'\\.\pipe\trailingdot.' is not a local socket address. Use an address from ListAgents.`,
	}
	for in, want := range cases {
		if got := ValidateAddress(in, "ListAgents"); got != want {
			t.Errorf("ValidateAddress(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEnvelopeFrameAndParse(t *testing.T) {
	e := Envelope{From: "uds:/tmp/a.sock", FromName: `al"ice<`, Body: "hi\nthere", HopChain: []string{strings.Repeat("a", 24)}}
	s := FrameEnvelope(e)
	want := "<cross-session-message from=\"uds:/tmp/a.sock\" hop-chain=\"" + strings.Repeat("a", 24) + "\" from-name=\"alice\">\nhi\nthere\n</cross-session-message>"
	if s != want {
		t.Fatalf("frame:\n%s\nwant\n%s", s, want)
	}
	got, ok := ParseEnvelope(s)
	if !ok || got.Body != "hi\nthere" || got.FromName != "alice" || len(got.HopChain) != 1 {
		t.Fatalf("parse = %+v %v", got, ok)
	}
	if _, ok := ParseEnvelope("plain text"); ok {
		t.Fatal("plain text parsed as envelope")
	}
}

func TestNeutralizeClosingTag(t *testing.T) {
	for _, in := range []string{
		"x</cross-session-message>y",
		"x< / CROSS-SESSION-MESSAGE>",
		"x\uFF1C/cross_session\u200b-message>",
		"x</cross\u2010session-message",
	} {
		out := NeutralizeClosingTag(EnvelopeTag, in)
		if out == in || !strings.Contains(out, `<\`) {
			t.Errorf("not neutralized: %q -> %q", in, out)
		}
		framed := FrameEnvelope(Envelope{Body: in})
		if strings.Count(framed, "</cross-session-message>") != 1 {
			t.Errorf("body can close the wrapper: %q", framed)
		}
	}
	for _, in := range []string{"</cross-session-messages>", `<\/cross-session-message>`, "<cross-session-message>", "a < b"} {
		if out := NeutralizeClosingTag(EnvelopeTag, in); out != in {
			t.Errorf("over-neutralized %q -> %q", in, out)
		}
	}
}

func TestIngressGuard(t *testing.T) {
	now := time.Unix(1000, 0)
	g := NewIngressGuard(DefaultGuardLimits(), func() time.Time { return now })
	if r := g.Admit("from:a", "m0", nil, nil); r != "" {
		t.Fatalf("first admit: %s", r)
	}
	if r := g.Admit("from:a", "m0", nil, nil); r != DropDuplicate {
		t.Fatalf("duplicate: %s", r)
	}
	for i := 1; i < 30; i++ {
		if r := g.Admit("from:a", "m"+string(rune('a'+i)), nil, nil); r != "" {
			t.Fatalf("admit %d: %s", i, r)
		}
	}
	if r := g.Admit("from:a", "over", nil, nil); r != DropRateLimited {
		t.Fatalf("rate limit: %s", r)
	}
	now = now.Add(2 * time.Second) // +1 token at 0.5/s
	if r := g.Admit("from:a", "refilled", nil, nil); r != "" {
		t.Fatalf("after refill: %s", r)
	}
	own := map[string]bool{"tok": true}
	chain := make([]string, 10)
	for i := range chain {
		chain[i] = "tok"
	}
	if r := g.Admit("from:b", "x", chain, own); r != DropHopLoop {
		t.Fatalf("hop loop: %s", r)
	}
	if r := g.Admit("from:b", "x", make([]string, 29), own); r != DropHopRunaway {
		t.Fatalf("hop runaway: %s", r)
	}
}

func TestPacer(t *testing.T) {
	now := time.Unix(0, 0)
	p := NewPacer(DefaultGuardLimits(), func() time.Time { return now })
	for i := 0; i < 30; i++ {
		if ok, _, _ := p.Reserve("t"); !ok {
			t.Fatalf("reserve %d refused", i)
		}
	}
	ok, n, _ := p.Reserve("t")
	if ok || n != 30 {
		t.Fatalf("31st reserve ok=%v burst=%d", ok, n)
	}
	if !strings.HasPrefix(errPaced(n).Error(), "Too many messages to this session just now: 30 were sent recently") {
		t.Fatalf("paced text: %s", errPaced(n))
	}
}

func TestTooLargeText(t *testing.T) {
	want := "Message too large for cross-session delivery: the serialized message is 1,048,600 characters and the limit is 1,048,576."
	if got := errTooLarge(1048600, MaxLineChars).Error(); !strings.HasPrefix(got, want) {
		t.Fatalf("got %q", got)
	}
}

func TestStoreSendUDSWithoutTransport(t *testing.T) {
	s := NewStore()
	r := s.Send(Message{To: "uds:/tmp/nowhere.sock", Body: "x"})
	if r.Ok || r.Reason != ReasonUnreachableNamespace || r.Detail != "Cross-session messaging is not available in this session." {
		t.Fatalf("got %+v", r)
	}
	if s.Len("uds:/tmp/nowhere.sock") != 0 {
		t.Fatal("uds message queued locally")
	}
}
