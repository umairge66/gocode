package sandbox

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// This file is the sandbox's network egress filter: one local listener that
// speaks both HTTP (CONNECT tunnels and absolute-URI forwarding) and SOCKS5,
// and lets a sandboxed command reach only the hosts the allow list names.
//
// The proxy is in-process Go, started on demand for the session and closed with
// it. It forwards only the connections a sandboxed command itself opens, to the
// hosts the user's settings allow; it never originates traffic of its own.

// ProxyPolicy is the filter the proxy applies to every connection.
type ProxyPolicy struct {
	// AllowedDomains / DeniedDomains are the network.allowedDomains /
	// deniedDomains entries (plus the WebFetch(domain:…) rules folded in by the
	// settings layer). A denied entry wins over an allowed one.
	AllowedDomains []string
	DeniedDomains  []string
	// StrictAllowlist denies a host neither list names without consulting Ask.
	StrictAllowlist bool
	// Ask, when set, decides a host neither list names (the interactive
	// network prompt). Nil denies it.
	Ask func(host string, port int) bool

	// Resolve, Dial and LocalAddrs are seams for tests; nil uses the system
	// resolver, a net.Dialer and the machine's interface addresses.
	Resolve    func(ctx context.Context, host string) ([]netip.Addr, error)
	Dial       func(ctx context.Context, network, address string) (net.Conn, error)
	LocalAddrs func() []netip.Addr
	// Logf receives debug lines (allow / deny decisions). Nil discards them.
	Logf func(format string, args ...any)
}

// Proxy is a running filtering proxy.
type Proxy struct {
	policy ProxyPolicy
	tcp    net.Listener
	httpLn *chanListener
	srv    *http.Server

	mu        sync.Mutex
	listeners []net.Listener
	conns     map[net.Conn]struct{}
	closed    bool
	wg        sync.WaitGroup
}

// Denial texts: "Connection blocked by network allowlist" with X-Proxy-Error
// "blocked-by-allowlist"; "blocked-by-sandbox-runtime" for a refused resolved
// address.
const (
	blockedByAllowlistBody   = "Connection blocked by network allowlist"
	blockedByAllowlistHeader = "blocked-by-allowlist"
	blockedByRuntimeHeader   = "blocked-by-sandbox-runtime"
)

// dialTimeout bounds one upstream connect.
const dialTimeout = 30 * time.Second

// StartProxy starts the proxy on an ephemeral 127.0.0.1 port.
func StartProxy(policy ProxyPolicy) (*Proxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("sandbox proxy: %w", err)
	}
	p := &Proxy{
		policy: policy,
		tcp:    ln,
		httpLn: newChanListener(ln.Addr()),
		conns:  map[net.Conn]struct{}{},
	}
	p.srv = &http.Server{
		Handler:           http.HandlerFunc(p.serveHTTP),
		ReadHeaderTimeout: 60 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		_ = p.srv.Serve(p.httpLn)
	}()
	p.serve(ln)
	p.logf("Mux proxy (HTTP+SOCKS) listening on localhost:%d", p.Port())
	return p, nil
}

// Port is the TCP port the proxy listens on (127.0.0.1).
func (p *Proxy) Port() int {
	return p.tcp.Addr().(*net.TCPAddr).Port
}

// ServeUnix additionally serves the proxy on a Unix-domain socket at path (the
// Linux bridge socket bound into bubblewrap). The socket is created 0600.
func (p *Proxy) ServeUnix(path string) error {
	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("sandbox proxy bridge socket: %w", err)
	}
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(true)
	}
	// Owner-only, whatever the umask (the directory is 0700 as well).
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return fmt.Errorf("sandbox proxy bridge socket: %w", err)
	}
	p.serve(ln)
	return nil
}

// serve accepts on ln until Close.
func (p *Proxy) serve(ln net.Listener) {
	p.mu.Lock()
	p.listeners = append(p.listeners, ln)
	p.mu.Unlock()
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if !p.track(c) {
				_ = c.Close()
				return
			}
			go p.route(c)
		}
	}()
}

func (p *Proxy) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.conns[c] = struct{}{}
	return true
}

func (p *Proxy) untrack(c net.Conn) {
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
}

// route peeks the first byte: 0x05 is a SOCKS5 greeting, anything else HTTP.
func (p *Proxy) route(c net.Conn) {
	br := bufio.NewReader(c)
	_ = c.SetReadDeadline(time.Now().Add(60 * time.Second))
	first, err := br.Peek(1)
	_ = c.SetReadDeadline(time.Time{})
	if err != nil {
		p.untrack(c)
		_ = c.Close()
		return
	}
	bc := &bufferedConn{Conn: c, r: br}
	if first[0] == 0x05 {
		defer p.untrack(c)
		defer c.Close()
		p.serveSOCKS(bc)
		return
	}
	// The HTTP server owns the connection from here; it is untracked when the
	// server closes it (trackedConn.Close).
	if !p.httpLn.deliver(&trackedConn{bufferedConn: bc, done: func() { p.untrack(c) }}) {
		p.untrack(c)
		_ = c.Close()
	}
}

// Close stops the proxy and drops every open connection.
func (p *Proxy) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	lns := p.listeners
	conns := make([]net.Conn, 0, len(p.conns))
	for c := range p.conns {
		conns = append(conns, c)
	}
	p.mu.Unlock()
	for _, ln := range lns {
		_ = ln.Close()
	}
	_ = p.httpLn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	_ = p.srv.Shutdown(ctx)
	cancel()
	for _, c := range conns {
		_ = c.Close()
	}
	p.wg.Wait()
	return nil
}

func (p *Proxy) logf(format string, args ...any) {
	if p.policy.Logf != nil {
		p.policy.Logf(format, args...)
	}
}

// Allowed applies the host filter to one destination.
func (p *Proxy) Allowed(host string, port int) (bool, string) {
	d := p.decide(host, port)
	return d.ok, d.reason
}

// hostDecision is the filter's verdict on one destination: the canonical host
// that was matched (and is the one dialled), and whether the destination was
// named exactly — by a non-wildcard allowedDomains entry or an interactive
// approval — rather than through "*" / "*.domain".
type hostDecision struct {
	ok     bool
	reason string
	host   string
	exact  bool
}

// decide is Allowed with the canonical host and the exact-match bit.
//
// Hardening: the host is canonicalized once
// (case, trailing dot, IDNA, legacy IPv4 spellings — canonicalHost) and that
// same string is both matched and dialled.
func (p *Proxy) decide(host string, port int) hostDecision {
	h := canonicalHost(host)
	if h == "" || port <= 0 || port > 65535 {
		p.logf("Denying malformed host: %q:%d", host, port)
		return hostDecision{reason: "malformed host"}
	}
	for _, d := range p.policy.DeniedDomains {
		if DomainMatches(h, port, d) {
			p.logf("Denied by config rule: %s:%d", h, port)
			return hostDecision{reason: "host is on the deny list", host: h}
		}
	}
	for _, a := range p.policy.AllowedDomains {
		if DomainMatches(h, port, a) {
			p.logf("Allowed by config rule: %s:%d", h, port)
			pat := ParseDomainPattern(a).Host
			return hostDecision{ok: true, host: h, exact: pat != "*" && !strings.HasPrefix(pat, "*.")}
		}
	}
	if p.policy.StrictAllowlist || p.policy.Ask == nil {
		p.logf("No matching config rule, denying: %s:%d", h, port)
		return hostDecision{reason: "host is not on the allow list", host: h}
	}
	if p.policy.Ask(h, port) {
		p.logf("User allowed: %s:%d", h, port)
		return hostDecision{ok: true, host: h, exact: true}
	}
	p.logf("User denied: %s:%d", h, port)
	return hostDecision{reason: "user denied", host: h}
}

// addressDeniedError is a destination name that resolved only to addresses
// the proxy may not dial ("Connection to <host> blocked: resolved to
// <reason>").
type addressDeniedError struct {
	host   string
	reason string
}

func (e *addressDeniedError) Error() string {
	return "Connection to " + e.host + " blocked: resolved to " + e.reason
}

// dialFiltered connects to host:port after the resolved-address check.
func (p *Proxy) dialFiltered(ctx context.Context, host string, port int, exact bool) (net.Conn, error) {
	dial := p.policy.Dial
	if dial == nil {
		d := &net.Dialer{Timeout: dialTimeout}
		dial = d.DialContext
	}
	h := canonicalHost(host)
	if h == "" {
		return nil, &addressDeniedError{host: host, reason: "a malformed host"}
	}
	local := p.policy.LocalAddrs
	if local == nil {
		local = localAddresses
	}
	allowed := ipRangeEntries(p.policy.AllowedDomains)
	denied := ipRangeEntries(p.policy.DeniedDomains)
	if isIPLiteral(h) {
		// An IP-literal destination still has to clear the address floor: a
		// deny-listed range, a cloud-metadata / loopback / link-local /
		// private / this-host address, unless the allow list named that range.
		// Without this, a broad allowlist (e.g. allowedDomains ["*"]) would let
		// a sandboxed command reach 169.254.169.254, host loopback services or
		// a LAN peer by dialing the literal IP, and a deniedDomains CIDR would
		// not apply to a literal IP at all (DomainMatches is a string compare).
		addr, perr := netip.ParseAddr(stripZone(h))
		if perr != nil {
			return nil, perr
		}
		if why := ipLiteralDenial(addr, port, allowed, denied, local()); why != "" {
			p.logf("Denied literal address %s:%d (%s)", h, port, why)
			return nil, &addressDeniedError{host: h, reason: why}
		}
		return dial(ctx, "tcp", net.JoinHostPort(addr.Unmap().String(), strconv.Itoa(port)))
	}
	resolve := p.policy.Resolve
	if resolve == nil {
		resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	addrs, err := resolve(ctx, h)
	if err != nil {
		return nil, err
	}
	own := local()
	var reasons []string
	var lastErr error
	tried := false
	for _, a := range addrs {
		if why := addressDenial(h, a, port, allowed, denied, own, exact); why != "" {
			reasons = append(reasons, why)
			continue
		}
		tried = true
		c, err := dial(ctx, "tcp", net.JoinHostPort(a.Unmap().String(), strconv.Itoa(port)))
		if err == nil {
			return c, nil
		}
		lastErr = err
	}
	if !tried && len(reasons) > 0 {
		p.logf("Denied address(es) for %s:%d (%s)", h, port, strings.Join(uniqStrings(reasons), "; "))
		return nil, &addressDeniedError{host: h, reason: strings.Join(uniqStrings(reasons), " / ")}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("getaddrinfo ENOTFOUND %s", h)
	}
	return nil, lastErr
}

// --- HTTP -------------------------------------------------------------------

// denyResponse renders the raw refusal written on a CONNECT.
func denyResponse(body, header, status string) string {
	if status == "" {
		status = "Forbidden"
	}
	return "HTTP/1.1 403 " + sanitizeStatus(status) + "\r\n" +
		"Content-Type: text/plain\r\n" +
		"X-Proxy-Error: " + header + "\r\n\r\n" + body
}

func sanitizeStatus(s string) string {
	return strings.Map(func(r rune) rune {
		if r < ' ' || r > '~' {
			return '?'
		}
		return r
	}, s)
}

func (p *Proxy) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.serveConnect(w, r)
		return
	}
	p.serveForward(w, r)
}

// serveConnect tunnels a CONNECT to an allowed host.
func (p *Proxy) serveConnect(w http.ResponseWriter, r *http.Request) {
	// The tunnel target is the CONNECT request-target (authority form), never
	// the Host header: a mismatched Host cannot steer the filter. Go's server
	// also accepts a "/path" CONNECT target (taking the host from the Host
	// header); that form is refused.
	target := ""
	if r.URL != nil && !strings.HasPrefix(r.RequestURI, "/") {
		target = r.URL.Host
	}
	host, portStr, err := net.SplitHostPort(target)
	port, perr := strconv.Atoi(portStr)
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
		return
	}
	client, rw, herr := hj.Hijack()
	if herr != nil {
		return
	}
	defer client.Close()
	if err != nil || perr != nil || port <= 0 || port > 65535 {
		_, _ = io.WriteString(client, "HTTP/1.1 400 Bad Request\r\n\r\n")
		return
	}
	dec := p.decide(host, port)
	if !dec.ok {
		p.logf("Connection blocked to %s:%d", host, port)
		_, _ = io.WriteString(client, denyResponse(blockedByAllowlistBody, blockedByAllowlistHeader, ""))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), dialTimeout)
	upstream, err := p.dialFiltered(ctx, dec.host, port, dec.exact)
	cancel()
	if err != nil {
		var ad *addressDeniedError
		if errors.As(err, &ad) {
			_, _ = io.WriteString(client, denyResponse(ad.Error(), blockedByRuntimeHeader, ad.reason))
			return
		}
		p.logf("CONNECT tunnel failed: %v", err)
		_, _ = io.WriteString(client, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer upstream.Close()
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	// Bytes the client sent right behind the CONNECT head are still in the
	// server's reader: forward them first.
	if n := rw.Reader.Buffered(); n > 0 {
		head, _ := rw.Reader.Peek(n)
		if _, err := upstream.Write(head); err != nil {
			return
		}
	}
	pipe(client, upstream)
}

// hopHeaders are dropped when forwarding (RFC 9110 §7.6.1).
var hopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

// serveForward forwards a plain-HTTP absolute-URI request to an allowed host.
func (p *Proxy) serveForward(w http.ResponseWriter, r *http.Request) {
	if r.URL == nil || r.URL.Host == "" {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "This is a proxy: send absolute-URI requests or CONNECT.\n")
		return
	}
	host := r.URL.Hostname()
	port := 80
	if r.URL.Scheme == "https" {
		port = 443
	}
	if ps := r.URL.Port(); ps != "" {
		n, err := strconv.Atoi(ps)
		if err != nil {
			http.Error(w, "bad port", http.StatusBadRequest)
			return
		}
		port = n
	}
	dec := p.decide(host, port)
	if !dec.ok {
		p.logf("HTTP request blocked to %s:%d", host, port)
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("X-Proxy-Error", blockedByAllowlistHeader)
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, blockedByAllowlistBody+"\n")
		return
	}
	tr := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return p.dialFiltered(ctx, dec.host, port, dec.exact)
		},
		ForceAttemptHTTP2:     false,
		ResponseHeaderTimeout: 5 * time.Minute,
	}
	defer tr.CloseIdleConnections()
	out := r.Clone(r.Context())
	out.RequestURI = ""
	// The upstream sees the host that was checked, never a different Host
	// header (which would let a request ride an allowed connection to another
	// virtual host on the same server).
	out.Host = r.URL.Host
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	resp, err := tr.RoundTrip(out)
	if err != nil {
		var ad *addressDeniedError
		if errors.As(err, &ad) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("X-Proxy-Error", blockedByRuntimeHeader)
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, ad.Error()+"\n")
			return
		}
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, h := range hopHeaders {
		resp.Header.Del(h)
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// --- SOCKS5 -----------------------------------------------------------------

// SOCKS5 reply codes.
const (
	socksGranted          = 0
	socksNotAllowed       = 2
	socksHostUnreachable  = 4
	socksCmdNotSupported  = 7
	socksAddrNotSupported = 8
)

func socksReply(code byte) []byte { return []byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0} }

// serveSOCKS handles one SOCKS5 client: no-auth greeting, a CONNECT request,
// the host filter, then a tunnel.
func (p *Proxy) serveSOCKS(c net.Conn) {
	_ = c.SetDeadline(time.Now().Add(60 * time.Second))
	var hdr [2]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil || hdr[0] != 5 || hdr[1] == 0 || hdr[1] > 128 {
		return
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return
	}
	noAuth := false
	for _, m := range methods {
		if m == 0 {
			noAuth = true
		}
	}
	if !noAuth {
		_, _ = c.Write([]byte{5, 255})
		return
	}
	if _, err := c.Write([]byte{5, 0}); err != nil {
		return
	}
	var req [4]byte
	if _, err := io.ReadFull(c, req[:]); err != nil || req[0] != 5 {
		return
	}
	var host string
	switch req[3] {
	case 1:
		var b [4]byte
		if _, err := io.ReadFull(c, b[:]); err != nil {
			return
		}
		host = netip.AddrFrom4(b).String()
	case 3:
		var l [1]byte
		if _, err := io.ReadFull(c, l[:]); err != nil {
			return
		}
		name := make([]byte, l[0])
		if _, err := io.ReadFull(c, name); err != nil {
			return
		}
		host = string(name)
	case 4:
		var b [16]byte
		if _, err := io.ReadFull(c, b[:]); err != nil {
			return
		}
		host = netip.AddrFrom16(b).String()
	default:
		_, _ = c.Write(socksReply(socksAddrNotSupported))
		return
	}
	var pb [2]byte
	if _, err := io.ReadFull(c, pb[:]); err != nil {
		return
	}
	port := int(binary.BigEndian.Uint16(pb[:]))
	if req[1] != 1 {
		_, _ = c.Write(socksReply(socksCmdNotSupported))
		return
	}
	dec := p.decide(host, port)
	if !dec.ok {
		p.logf("Connection blocked to %s:%d", host, port)
		_, _ = c.Write(socksReply(socksNotAllowed))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	upstream, err := p.dialFiltered(ctx, dec.host, port, dec.exact)
	cancel()
	if err != nil {
		var ad *addressDeniedError
		code := byte(socksHostUnreachable)
		if errors.As(err, &ad) {
			code = socksNotAllowed
		}
		p.logf("SOCKS connect to %s:%d failed: %v", host, port, err)
		_, _ = c.Write(socksReply(code))
		return
	}
	defer upstream.Close()
	if _, err := c.Write(socksReply(socksGranted)); err != nil {
		return
	}
	_ = c.SetDeadline(time.Time{})
	pipe(c, upstream)
}

// pipe copies both ways until either side closes.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	// Give the other direction a moment to drain a half-closed stream, then
	// tear both down.
	select {
	case <-done:
	case <-time.After(30 * time.Second):
	}
	_ = a.Close()
	_ = b.Close()
}

func uniqStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// --- plumbing ---------------------------------------------------------------

// bufferedConn is a net.Conn whose reads drain a bufio.Reader first (the peeked
// mux byte stays in the stream).
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// CloseWrite half-closes the underlying connection when it supports it.
func (b *bufferedConn) CloseWrite() error {
	if cw, ok := b.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// trackedConn runs done once when the HTTP server closes the connection.
type trackedConn struct {
	*bufferedConn
	once sync.Once
	done func()
}

func (t *trackedConn) Close() error {
	t.once.Do(t.done)
	return t.bufferedConn.Close()
}

// chanListener feeds connections the mux routed to HTTP into http.Server.
type chanListener struct {
	addr   net.Addr
	ch     chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newChanListener(addr net.Addr) *chanListener {
	return &chanListener{addr: addr, ch: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *chanListener) deliver(c net.Conn) bool {
	select {
	case l.ch <- c:
		return true
	case <-l.closed:
		return false
	}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *chanListener) Addr() net.Addr { return l.addr }
