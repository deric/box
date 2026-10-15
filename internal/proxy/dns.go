package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"
)

// DNS goes through the proxy too: inside the sandbox a DNSForwarder listens
// on 127.0.0.1:53 and sends each query to the proxy as a POST of the DNS
// wire message (the RFC 8484 format), which the Server's Resolver answers
// with the host's nameservers. The transport the client used is carried in
// DNSTransportHeader so that truncation behaves as it would without box.
const (
	// DNSPath is the proxy's DNS relay endpoint.
	DNSPath = "/dns-query"
	// DNSContentType is the media type of a DNS message in an HTTP body.
	DNSContentType = "application/dns-message"
	// DNSTransportHeader names the transport the query arrived on inside
	// the sandbox: "udp" or "tcp".
	DNSTransportHeader = "Box-Transport"
	// DNSMaxMessage is the largest DNS message relayed, the limit of the
	// length-prefixed TCP framing.
	DNSMaxMessage = 65535
)

// DefaultResolvConf is where the host's nameservers are read from.
var DefaultResolvConf = "/etc/resolv.conf"

// Resolver answers relayed DNS queries with the nameservers of the host,
// read from ResolvConf (DefaultResolvConf when empty) on every query so
// that a changed network configuration is picked up. Without any
// nameserver there, 127.0.0.1:53 is used, as the libc resolver does.
type Resolver struct {
	ResolvConf string
	// Timeout bounds one exchange with one nameserver; zero means 2s.
	Timeout time.Duration
}

// Nameservers returns the nameservers to query, in order, as host:port.
func (r *Resolver) Nameservers() []string {
	path := r.ResolvConf
	if path == "" {
		path = DefaultResolvConf
	}
	data, _ := os.ReadFile(path)
	servers, _ := ParseResolvConf(data)
	if len(servers) == 0 {
		servers = []string{"127.0.0.1:53"}
	}
	return servers
}

// ParseResolvConf splits a resolv.conf into its nameservers (as host:port,
// invalid addresses dropped) and its remaining directives (search, domain,
// options, ...), without comments or blank lines. Beyond what the libc
// resolver reads, a nameserver may carry a port: "127.0.0.1:5353" or
// "[::1]:5353".
func ParseResolvConf(data []byte) (nameservers, rest []string) {
	for _, line := range strings.Split(string(data), "\n") {
		if i := strings.IndexAny(line, "#;"); i >= 0 {
			line = line[:i]
		}
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if f[0] != "nameserver" {
			rest = append(rest, strings.Join(f, " "))
			continue
		}
		if len(f) < 2 {
			continue
		}
		if addr, err := netip.ParseAddr(f[1]); err == nil {
			nameservers = append(nameservers, net.JoinHostPort(addr.String(), "53"))
		} else if ap, err := netip.ParseAddrPort(f[1]); err == nil {
			nameservers = append(nameservers, ap.String())
		}
	}
	return nameservers, rest
}

func (r *Resolver) timeout() time.Duration {
	if r.Timeout > 0 {
		return r.Timeout
	}
	return 2 * time.Second
}

// Exchange sends query to the nameservers in turn, over TCP or UDP as the
// client did, and returns the first response.
func (r *Resolver) Exchange(ctx context.Context, query []byte, tcp bool) ([]byte, error) {
	if len(query) < 12 {
		return nil, fmt.Errorf("dns: query too short (%d bytes)", len(query))
	}
	var last error
	for _, ns := range r.Nameservers() {
		resp, err := exchange(ctx, ns, query, tcp, r.timeout())
		if err == nil {
			return resp, nil
		}
		last = fmt.Errorf("%s: %w", ns, err)
		if ctx.Err() != nil {
			break
		}
	}
	return nil, last
}

func exchange(ctx context.Context, ns string, query []byte, tcp bool, timeout time.Duration) ([]byte, error) {
	network := "udp"
	if tcp {
		network = "tcp"
	}
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, network, ns)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	if tcp {
		if err := writeFramed(conn, query); err != nil {
			return nil, err
		}
		return readFramed(conn)
	}
	if _, err := conn.Write(query); err != nil {
		return nil, err
	}
	buf := make([]byte, DNSMaxMessage)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return nil, err
		}
		// A late answer to an earlier query on a reused port is not ours.
		if n >= 12 && bytes.Equal(buf[:2], query[:2]) {
			return append([]byte(nil), buf[:n]...), nil
		}
	}
}

// writeFramed writes msg with the two-byte length prefix of DNS over TCP.
func writeFramed(w io.Writer, msg []byte) error {
	if len(msg) > DNSMaxMessage {
		return fmt.Errorf("dns: message of %d bytes is too long", len(msg))
	}
	buf := make([]byte, 2+len(msg))
	binary.BigEndian.PutUint16(buf, uint16(len(msg)))
	copy(buf[2:], msg)
	_, err := w.Write(buf)
	return err
}

// readFramed reads one length-prefixed DNS message.
func readFramed(r io.Reader) ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	msg := make([]byte, binary.BigEndian.Uint16(hdr[:]))
	if _, err := io.ReadFull(r, msg); err != nil {
		return nil, err
	}
	return msg, nil
}

// dns handles a relayed query (POST DNSPath) with s.DNS.
func (s *Server) dns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "box proxy: POST required", http.StatusMethodNotAllowed)
		return
	}
	query, err := io.ReadAll(io.LimitReader(r.Body, DNSMaxMessage+1))
	if err != nil {
		http.Error(w, "box proxy: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(query) > DNSMaxMessage {
		http.Error(w, "box proxy: DNS message too long", http.StatusRequestEntityTooLarge)
		return
	}
	tcp := r.Header.Get(DNSTransportHeader) == "tcp"
	resp, err := s.DNS.Exchange(r.Context(), query, tcp)
	if err != nil {
		s.logf("dns: %v", err)
		http.Error(w, "box proxy: dns: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", DNSContentType)
	_, _ = w.Write(resp)
}

// DNSForwarder is the resolver inside the sandbox: it serves DNS on
// listeners bound to 127.0.0.1:53 (see package sandbox) and relays each
// query to the proxy over the unix socket at Socket. When the proxy cannot
// answer, the client gets SERVFAIL rather than a timeout.
type DNSForwarder struct {
	Socket string
	// Timeout bounds one relayed query; zero means 8s.
	Timeout time.Duration

	once   sync.Once
	client *http.Client
}

func (f *DNSForwarder) init() {
	f.once.Do(func() {
		timeout := f.Timeout
		if timeout == 0 {
			timeout = 8 * time.Second
		}
		f.client = &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", f.Socket)
				},
				MaxIdleConns:    8,
				IdleConnTimeout: time.Minute,
			},
		}
	})
}

// Query relays one DNS message and returns the response, or an error when
// the proxy did not answer.
func (f *DNSForwarder) Query(ctx context.Context, msg []byte, tcp bool) ([]byte, error) {
	f.init()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://box"+DNSPath, bytes.NewReader(msg))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", DNSContentType)
	transport := "udp"
	if tcp {
		transport = "tcp"
	}
	req.Header.Set(DNSTransportHeader, transport)
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, DNSMaxMessage+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("proxy: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if len(body) < 12 || len(body) > DNSMaxMessage {
		return nil, fmt.Errorf("proxy: malformed DNS response of %d bytes", len(body))
	}
	return body, nil
}

// answer returns the response to msg: the relayed one, or SERVFAIL.
func (f *DNSForwarder) answer(msg []byte, tcp bool) []byte {
	if len(msg) < 12 {
		return nil
	}
	resp, err := f.Query(context.Background(), msg, tcp)
	if err != nil {
		return ServFail(msg)
	}
	return resp
}

// ServFail builds the SERVFAIL response to query: its header with QR and RA
// set and the response code SERVFAIL, followed by its question section as
// it was.
func ServFail(query []byte) []byte {
	if len(query) < 12 {
		return nil
	}
	resp := append([]byte(nil), query...)
	resp[2] = resp[2]&0x79 | 0x80 // QR=1; keep opcode and RD; clear AA, TC
	resp[3] = 0x82                // RA=1, Z/AD/CD=0, RCODE=2 (SERVFAIL)
	binary.BigEndian.PutUint16(resp[6:], 0)
	binary.BigEndian.PutUint16(resp[8:], 0)
	return resp
}

// ServeUDP answers queries on pc until it is closed.
func (f *DNSForwarder) ServeUDP(pc net.PacketConn) error {
	buf := make([]byte, DNSMaxMessage)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		msg := append([]byte(nil), buf[:n]...)
		go func() {
			if resp := f.answer(msg, false); resp != nil {
				_, _ = pc.WriteTo(resp, addr)
			}
		}()
	}
}

// ServeTCP answers queries on connections accepted from ln until it is
// closed. A connection may carry several queries; it is dropped after 10s
// without one.
func (f *DNSForwarder) ServeTCP(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go f.serveConn(c)
	}
}

func (f *DNSForwarder) serveConn(c net.Conn) {
	defer func() { _ = c.Close() }()
	for {
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		msg, err := readFramed(c)
		if err != nil {
			return
		}
		resp := f.answer(msg, true)
		if resp == nil {
			return
		}
		if err := writeFramed(c, resp); err != nil {
			return
		}
	}
}
