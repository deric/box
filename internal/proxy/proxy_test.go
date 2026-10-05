package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func TestAllowlistMatch(t *testing.T) {
	a := Allowlist{"api.anthropic.com", "*.github.com"}
	for _, tc := range []struct {
		host           string
		allowed, exact bool
	}{
		{"api.anthropic.com", true, true},
		{"API.Anthropic.COM.", true, true},
		{"anthropic.com", false, false},
		{"github.com", false, false},
		{"api.github.com", true, false},
		{"a.b.github.com", true, false},
		{"evilgithub.com", false, false},
		{"127.0.0.1", false, false},
	} {
		allowed, exact := a.Match(tc.host)
		if allowed != tc.allowed || exact != tc.exact {
			t.Errorf("Match(%q) = %v, %v; want %v, %v", tc.host, allowed, exact, tc.allowed, tc.exact)
		}
	}
	if ok, _ := (Allowlist{"*"}).Match("anything.example"); !ok {
		t.Error("* should match everything")
	}
	if ok, _ := (Allowlist{}).Match("x"); ok {
		t.Error("empty allowlist should deny")
	}
}

// start runs a Server on a unix socket and returns a client that uses it as
// its proxy, plus the buffer the server logs to.
func start(t *testing.T, allow Allowlist) (*http.Client, *bytes.Buffer) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "p.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	logs := &bytes.Buffer{}
	srv := &Server{Allow: allow, Log: log.New(logs, "", 0)}
	go func() { _ = srv.Serve(ln) }()

	// The transport dials the proxy over the unix socket but talks to it as
	// if it were http://proxy, just like a forwarder on loopback would.
	tr := &http.Transport{
		Proxy: func(*http.Request) (*url.URL, error) { return url.Parse("http://proxy") },
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test server certificate
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}, logs
}

func hello(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Upstream", "yes")
	_, _ = io.WriteString(w, "hello "+r.URL.Path+" "+r.Header.Get("Proxy-Connection"))
}

func TestServerPlainHTTP(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(hello))
	defer up.Close()
	host, _, _ := net.SplitHostPort(up.Listener.Addr().String())
	client, logs := start(t, Allowlist{host})

	resp, err := client.Get(up.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "hello /x " || resp.Header.Get("X-Upstream") != "yes" {
		t.Errorf("got %d %q %v", resp.StatusCode, body, resp.Header)
	}

	resp, err = client.Get("http://denied.example/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "denied.example") {
		t.Errorf("got %d %q, want 403 naming the host", resp.StatusCode, body)
	}
	if !strings.Contains(logs.String(), "denied GET http://denied.example/") {
		t.Errorf("log = %q", logs.String())
	}
}

func TestServerConnect(t *testing.T) {
	up := httptest.NewTLSServer(http.HandlerFunc(hello))
	defer up.Close()
	host, _, _ := net.SplitHostPort(up.Listener.Addr().String())
	client, logs := start(t, Allowlist{host})

	resp, err := client.Get(up.URL + "/tls")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "hello /tls " {
		t.Errorf("got %d %q", resp.StatusCode, body)
	}

	if _, err := client.Get("https://denied.example/"); err == nil || !strings.Contains(err.Error(), "Forbidden") {
		t.Errorf("CONNECT to a denied host should fail with 403, got %v", err)
	}
	if !strings.Contains(logs.String(), "denied CONNECT denied.example:443") {
		t.Errorf("log = %q", logs.String())
	}
}

func TestServerWildcardRefusesLoopback(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(hello))
	defer up.Close()
	client, logs := start(t, Allowlist{"*"})
	_, port, _ := net.SplitHostPort(up.Listener.Addr().String())

	resp, err := client.Get("http://localhost:" + port + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(logs.String(), "local address") {
		t.Errorf("got %d, log %q; want 502 refusing a loopback address", resp.StatusCode, logs.String())
	}
}

func TestForward(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "e.sock")
	echo, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = echo.Close() }()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = Forward(ln, sock) }()
	defer func() { _ = ln.Close() }()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(c, "ping"); err != nil {
		t.Fatal(err)
	}
	_ = c.(*net.TCPConn).CloseWrite()
	got, _ := io.ReadAll(c)
	_ = c.Close()
	if string(got) != "ping" {
		t.Errorf("echoed %q", got)
	}
}
