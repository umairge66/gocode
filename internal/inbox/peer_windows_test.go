//go:build windows

package inbox

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestDefaultPipeNameShape(t *testing.T) {
	p := DefaultSocketPath()
	if !regexp.MustCompile(`^\\\\\.\\pipe\\cc-msg-[0-9a-f]{32}$`).MatchString(p) {
		t.Fatalf("pipe name = %q", p)
	}
	if localPipeName(p) == "" || !strings.HasPrefix(canonicalEndpoint(strings.ToUpper(p)), `\\.\pipe\cc-msg-`) {
		t.Fatalf("pipe name not local/canonical: %q", p)
	}
}

func TestPipeRoundTrip(t *testing.T) {
	recv, err := Listen(ListenerConfig{Store: NewStore()})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer recv.Close()
	self, err := Listen(ListenerConfig{Store: NewStore()})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer self.Close()
	id, err := NewPeerSender(self).Send(context.Background(), recv.Address(), "hello", PeerSendOptions{})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for recv.cfg.Store.Len(MainRecipient) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	m, ok := recv.cfg.Store.Fetch(MainRecipient)
	if !ok || m.ID != id || m.From != self.Address() {
		t.Fatalf("message = %+v ok=%v", m, ok)
	}
	if _, ok := ParseEnvelope(m.Body); !ok {
		t.Fatalf("not framed: %q", m.Body)
	}
	if _, err := NewPeerSender(self).Send(context.Background(), self.Address(), "x", PeerSendOptions{}); err == nil || sendReasonFor(err) != ReasonSelfTarget {
		t.Fatalf("self-target err = %v", err)
	}
}

func TestPipeGone(t *testing.T) {
	_, err := NewPeerSender(nil).Send(context.Background(), UDSAddress(`\\.\pipe\cc-msg-`+strings.Repeat("0", 32)), "x", PeerSendOptions{})
	if err == nil || sendReasonFor(err) != ReasonPeerGone {
		t.Fatalf("err = %v", err)
	}
}
