package proxy

import (
	"bytes"
	"encoding/binary"
	"log"
	"net"
	"net/netip"
	"testing"
	"time"
)

// response builds a response to q with the given records appended to its
// answer section: each is (type, rdata, ttl), with the owner name as a
// compression pointer to the question.
func response(q []byte, rrs ...[3]any) []byte {
	i := 12
	for i < len(q) && q[i] != 0 {
		i += int(q[i]) + 1
	}
	r := append([]byte(nil), q[:i+5]...)
	r[2], r[3] = 0x81, 0x80
	binary.BigEndian.PutUint16(r[6:], uint16(len(rrs)))
	for _, rr := range rrs {
		typ, rdata, ttl := rr[0].(uint16), rr[1].([]byte), rr[2].(uint32)
		r = append(r, 0xc0, 0x0c)
		r = binary.BigEndian.AppendUint16(r, typ)
		r = binary.BigEndian.AppendUint16(r, 1)
		r = binary.BigEndian.AppendUint32(r, ttl)
		r = binary.BigEndian.AppendUint16(r, uint16(len(rdata)))
		r = append(r, rdata...)
	}
	return r
}

func TestQuestionName(t *testing.T) {
	if name, ok := QuestionName(query(1, "API.GitHub.com")); !ok || name != "api.github.com" {
		t.Errorf("QuestionName = %q, %v", name, ok)
	}
	for _, bad := range [][]byte{nil, {1, 2, 3}, make([]byte, 12), append(make([]byte, 12), 0x40, 'a')} {
		if _, ok := QuestionName(bad); ok {
			t.Errorf("QuestionName(%x) accepted", bad)
		}
	}
	// A pointer loop ends.
	loop := append(query(1, "a"), 0xc0, 12)
	binary.BigEndian.PutUint16(loop[4:], 1)
	loop[12], loop[13] = 0xc0, 12
	if _, ok := QuestionName(loop); ok {
		t.Error("pointer loop accepted")
	}
}

func TestAnswerAddrs(t *testing.T) {
	q := query(3, "www.github.com")
	v6 := netip.MustParseAddr("2606:50c0:8000::153").As16()
	resp := response(q,
		[3]any{uint16(5), []byte{6, 'g', 'i', 't', 'h', 'u', 'b', 0xc0, 0x10}, uint32(3600)}, // CNAME github.com
		[3]any{uint16(1), []byte{140, 82, 121, 3}, uint32(60)},
		[3]any{uint16(28), v6[:], uint32(300)},
		[3]any{uint16(16), []byte{3, 'f', 'o', 'o'}, uint32(1)},
	)
	got := AnswerAddrs(resp)
	want := []AddrRecord{{netip.MustParseAddr("140.82.121.3"), 60}, {netip.MustParseAddr("2606:50c0:8000::153"), 300}}
	if len(got) != len(want) {
		t.Fatalf("AnswerAddrs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("record %d = %v, want %v", i, got[i], want[i])
		}
	}
	// A truncated message yields what was complete.
	if got := AnswerAddrs(resp[:len(resp)-10]); len(got) != 2 {
		t.Errorf("truncated: %v", got)
	}
	if got := AnswerAddrs(resp[:30]); len(got) != 0 {
		t.Errorf("cut in the question: %v", got)
	}
	if AnswerAddrs(ServFail(q)) != nil {
		t.Error("SERVFAIL has addresses")
	}
}

// filterFixture returns a filter whose relay answers from the given
// nameserver, recording what it admits.
func filterFixture(t *testing.T, ns *fakeNS, allow ...string) (*DNSFilter, *[]string, *bytes.Buffer) {
	t.Helper()
	var admitted []string
	logs := &bytes.Buffer{}
	f := &DNSFilter{
		Allow: Allowlist(allow),
		Relay: startedRelay(t, ns),
		Admit: func(a netip.Addr, ttl time.Duration) error {
			admitted = append(admitted, a.String()+" "+ttl.String())
			return nil
		},
		Log: log.New(logs, "", 0),
	}
	return f, &admitted, logs
}

// startedRelay runs a proxy with a resolver on ns and returns a forwarder
// relaying to it.
func startedRelay(t *testing.T, ns *fakeNS) *DNSForwarder {
	t.Helper()
	sock := t.TempDir() + "/p.sock"
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{DNS: &Resolver{ResolvConf: resolvConfWith(t, "nameserver "+ns.addr)}}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = ln.Close() })
	return &DNSForwarder{Socket: sock, Timeout: 5 * time.Second}
}

func TestDNSFilter(t *testing.T) {
	ns := startFakeNS(t)
	f, admitted, logs := filterFixture(t, ns, "github.com", "*.example.org")

	// An allowed name: the upstream answer, its address admitted for the
	// record's TTL (the fake nameserver answers 192.0.2.1 with TTL 60).
	q := query(1, "github.com")
	resp := f.Answer(q, false)
	if !bytes.Equal(resp[:2], q[:2]) || !bytes.HasSuffix(resp, []byte{192, 0, 2, 1}) {
		t.Errorf("response %x", resp)
	}
	if len(*admitted) != 1 || (*admitted)[0] != "192.0.2.1 1m0s" {
		t.Errorf("admitted = %v", *admitted)
	}
	<-ns.seen

	// Another: NXDOMAIN without asking upstream, and a denied line that
	// box top counts like the proxy's.
	q = query(2, "evil.example.com")
	resp = f.Answer(q, true)
	if len(resp) != len(q) || resp[3]&0x0f != rcodeNXDomain || resp[2]&0x80 == 0 || binary.BigEndian.Uint16(resp[6:]) != 0 {
		t.Errorf("denied response %x", resp)
	}
	if len(*admitted) != 1 {
		t.Errorf("admitted after a denied name: %v", *admitted)
	}
	select {
	case tr := <-ns.seen:
		t.Errorf("denied name relayed over %s", tr)
	default:
	}
	if logs.String() != "denied DNS evil.example.com\n" {
		t.Errorf("log = %q", logs.String())
	}

	// A malformed query has no answer.
	if f.Answer([]byte{1, 2, 3}, false) != nil {
		t.Error("malformed query answered")
	}
}

func TestDNSFilterLocalAddresses(t *testing.T) {
	ns := startFakeNS(t)
	var admitted []string
	logs := &bytes.Buffer{}
	f := &DNSFilter{
		Allow: Allowlist{"*.example.org", "local.example.net"},
		Admit: func(a netip.Addr, ttl time.Duration) error {
			admitted = append(admitted, a.String())
			return nil
		},
		Log: log.New(logs, "", 0),
	}
	f.Relay = startedRelay(t, ns)
	// The policy is exercised on a synthetic answer: a loopback and a
	// link-local address along with a public one.
	q := query(5, "a.example.org")
	resp := response(q,
		[3]any{uint16(1), []byte{127, 0, 0, 1}, uint32(60)},
		[3]any{uint16(1), []byte{169, 254, 169, 254}, uint32(60)},
		[3]any{uint16(1), []byte{192, 0, 2, 7}, uint32(60)},
	)
	f.admitAll("a.example.org", false, resp)
	if len(admitted) != 1 || admitted[0] != "192.0.2.7" {
		t.Errorf("wildcard match admitted %v", admitted)
	}
	if !bytes.Contains(logs.Bytes(), []byte("127.0.0.1")) || !bytes.Contains(logs.Bytes(), []byte("169.254.169.254")) {
		t.Errorf("log = %q", logs.String())
	}
	admitted = nil
	f.admitAll("local.example.net", true, resp)
	if len(admitted) != 3 {
		t.Errorf("exact match admitted %v", admitted)
	}
}

func TestErrorResponse(t *testing.T) {
	q := query(9, "x.test")
	q[2] |= 0x04 // AA set by a confused client; cleared in the response
	r := errorResponse(q, rcodeNXDomain)
	if r[2] != 0x81 || r[3] != 0x83 || !bytes.Equal(r[12:], q[12:]) {
		t.Errorf("errorResponse = %x", r)
	}
	if errorResponse(nil, 1) != nil {
		t.Error("short query answered")
	}
}
