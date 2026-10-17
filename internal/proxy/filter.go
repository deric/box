package proxy

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// With the pasta backend the sandbox has a network interface of its own
// and the allowlist is enforced by name resolution: DNSFilter serves DNS
// inside the sandbox's network namespace, answers only queries for names
// on the allowlist (relaying them to the host's resolver through the
// proxy, like DNSForwarder) and admits every address it hands out through
// the firewall there, by calling Admit. Anything else is unreachable, as
// the firewall rejects connections to addresses that were never admitted.

// DNSFilter is the allowlisting resolver of a pasta sandbox (see above).
type DNSFilter struct {
	Allow Allowlist
	// Relay answers allowed queries with the host's resolver.
	Relay *DNSForwarder
	// Admit lets addr through the firewall for ttl at least; it is called
	// for every A and AAAA record of an allowed answer.
	Admit func(addr netip.Addr, ttl time.Duration) error
	// Log receives denied names and failures; nil discards them.
	Log *log.Logger
}

func (f *DNSFilter) logf(format string, args ...any) {
	if f.Log != nil {
		f.Log.Printf(format, args...)
	}
}

// Answer returns the response to query: the relayed answer for an allowed
// name with its addresses admitted, NXDOMAIN for any other name, SERVFAIL
// when the query cannot be relayed, and nil for a malformed query.
func (f *DNSFilter) Answer(query []byte, tcp bool) []byte {
	name, ok := QuestionName(query)
	if !ok {
		return nil
	}
	allowed, exact := f.Allow.Match(name)
	if !allowed {
		// The log line matches the proxy's denied requests, which box
		// top counts by target.
		f.logf("denied DNS %s", name)
		return errorResponse(query, rcodeNXDomain)
	}
	resp, err := f.Relay.Query(context.Background(), query, tcp)
	if err != nil {
		f.logf("dns %s: %v", name, err)
		return ServFail(query)
	}
	if err := f.admitAll(name, exact, resp); err != nil {
		return ServFail(query)
	}
	return resp
}

// admitAll admits the addresses of resp, an answer for name. Names that
// match the allowlist only through a wildcard must not resolve to
// loopback or link-local addresses, as with the proxy, so that a wildcard
// entry cannot be turned against the host; such addresses are skipped.
func (f *DNSFilter) admitAll(name string, exact bool, resp []byte) error {
	for _, rr := range AnswerAddrs(resp) {
		if !exact && (rr.Addr.IsLoopback() || rr.Addr.IsLinkLocalUnicast() || rr.Addr.IsUnspecified()) {
			f.logf("%s resolves to local address %s; list it explicitly to allow that", name, rr.Addr)
			continue
		}
		if f.Admit == nil {
			continue
		}
		if err := f.Admit(rr.Addr, time.Duration(rr.TTL)*time.Second); err != nil {
			f.logf("dns %s: admitting %s: %v", name, rr.Addr, err)
			return err
		}
	}
	return nil
}

// ServeUDP answers queries on pc until it is closed.
func (f *DNSFilter) ServeUDP(pc net.PacketConn) error {
	return (&dnsServer{answer: f.Answer}).serveUDP(pc)
}

// ServeTCP answers queries on connections accepted from ln until it is
// closed.
func (f *DNSFilter) ServeTCP(ln net.Listener) error {
	return (&dnsServer{answer: f.Answer}).serveTCP(ln)
}

// DNS record types and response codes used here.
const (
	typeA    = 1
	typeAAAA = 28

	rcodeNXDomain = 3
	rcodeServFail = 2
)

// AddrRecord is an address from the answer section of a DNS response with
// its time to live in seconds.
type AddrRecord struct {
	Addr netip.Addr
	TTL  uint32
}

// QuestionName returns the name the first question of msg asks about,
// lower-cased and without the trailing dot, and false for a message
// without a parsable question.
func QuestionName(msg []byte) (string, bool) {
	if len(msg) < 12 || binary.BigEndian.Uint16(msg[4:]) == 0 {
		return "", false
	}
	name, _, ok := readName(msg, 12)
	if !ok {
		return "", false
	}
	return name, true
}

// AnswerAddrs returns the A and AAAA records of msg's answer section, in
// order; other records and a malformed section yield nothing.
func AnswerAddrs(msg []byte) []AddrRecord {
	if len(msg) < 12 {
		return nil
	}
	qd, an := int(binary.BigEndian.Uint16(msg[4:])), int(binary.BigEndian.Uint16(msg[6:]))
	off := 12
	for range qd {
		_, next, ok := readName(msg, off)
		if !ok || next+4 > len(msg) {
			return nil
		}
		off = next + 4 // QTYPE, QCLASS
	}
	var out []AddrRecord
	for range an {
		_, next, ok := readName(msg, off)
		if !ok || next+10 > len(msg) {
			return out
		}
		typ := binary.BigEndian.Uint16(msg[next:])
		ttl := binary.BigEndian.Uint32(msg[next+4:])
		rdlen := int(binary.BigEndian.Uint16(msg[next+8:]))
		rdata := next + 10
		if rdata+rdlen > len(msg) {
			return out
		}
		switch {
		case typ == typeA && rdlen == 4:
			out = append(out, AddrRecord{netip.AddrFrom4([4]byte(msg[rdata : rdata+4])), ttl})
		case typ == typeAAAA && rdlen == 16:
			out = append(out, AddrRecord{netip.AddrFrom16([16]byte(msg[rdata : rdata+16])), ttl})
		}
		off = rdata + rdlen
	}
	return out
}

// readName decodes the domain name at off, following compression
// pointers, and returns it lower-cased without the trailing dot along with
// the offset just past the name as written at off.
func readName(msg []byte, off int) (name string, next int, ok bool) {
	var labels []string
	next = -1
	for hops := 0; hops < 64; hops++ {
		if off >= len(msg) {
			return "", 0, false
		}
		l := int(msg[off])
		switch {
		case l == 0:
			if next < 0 {
				next = off + 1
			}
			return strings.ToLower(strings.Join(labels, ".")), next, true
		case l&0xc0 == 0xc0:
			if off+1 >= len(msg) {
				return "", 0, false
			}
			if next < 0 {
				next = off + 2
			}
			off = int(binary.BigEndian.Uint16(msg[off:]) & 0x3fff)
		case l&0xc0 != 0:
			return "", 0, false
		default:
			if off+1+l > len(msg) {
				return "", 0, false
			}
			labels = append(labels, string(msg[off+1:off+1+l]))
			off += 1 + l
		}
	}
	return "", 0, false
}

// errorResponse builds the response to query with the given response code
// and no records: its header with QR and RA set, followed by its question
// section as it was.
func errorResponse(query []byte, rcode byte) []byte {
	if len(query) < 12 {
		return nil
	}
	resp := append([]byte(nil), query...)
	resp[2] = resp[2]&0x79 | 0x80 // QR=1; keep opcode and RD; clear AA, TC
	resp[3] = 0x80 | rcode&0x0f   // RA=1, Z/AD/CD=0
	binary.BigEndian.PutUint16(resp[6:], 0)
	binary.BigEndian.PutUint16(resp[8:], 0)
	binary.BigEndian.PutUint16(resp[10:], 0)
	return resp
}

// PastaPath is the proxy endpoint `box _netns` asks to attach pasta(1) to
// the network namespace it runs in: a POST with the form field pid, the
// process whose namespaces pasta joins. The proxy runs on the host, where
// pasta has to be started from (its sockets are the host's), while the
// namespace only exists once box is inside it.
const PastaPath = "/pasta"

// pasta handles a PastaPath request with s.Pasta.
func (s *Server) pasta(w http.ResponseWriter, r *http.Request) {
	if s.Pasta == nil {
		http.Error(w, "box proxy: pasta is not enabled for this sandbox", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "box proxy: POST required", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1024))
	if err != nil {
		http.Error(w, "box proxy: "+err.Error(), http.StatusBadRequest)
		return
	}
	form, err := url.ParseQuery(string(body))
	pid := 0
	if err == nil {
		pid, err = strconv.Atoi(form.Get("pid"))
	}
	if err != nil || pid <= 0 {
		http.Error(w, "box proxy: pasta: a pid is required", http.StatusBadRequest)
		return
	}
	if err := s.Pasta(pid); err != nil {
		s.logf("pasta: %v", err)
		http.Error(w, "box proxy: pasta: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RequestPasta asks the proxy at socket to attach pasta to the namespaces
// of process pid, waiting until it is up. The error carries the proxy's
// explanation when it refused or pasta failed.
func RequestPasta(ctx context.Context, socket string, pid int) error {
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}
	defer client.CloseIdleConnections()
	body := url.Values{"pid": {strconv.Itoa(pid)}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://box"+PastaPath, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		text := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(msg)), "box proxy: "))
		if text == "" {
			text = resp.Status
		}
		return errors.New(text)
	}
	return nil
}
