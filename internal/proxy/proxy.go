// Package proxy implements the allowlisting HTTP proxy a sandbox reaches the
// network through, and the forwarder that exposes it inside the sandbox.
//
// The sandbox has an empty network namespace. On the host, box runs Server
// on a unix socket that is bound into the sandbox; inside, Forward accepts
// TCP connections on loopback and relays them to that socket. Programs use
// it through HTTP_PROXY / HTTPS_PROXY, and only hosts on the allowlist can
// be reached.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Allowlist decides which destination hosts may be reached. An entry is a
// host name, compared case-insensitively, or "*.suffix", which matches any
// name under suffix at any depth (but not suffix itself). "*" matches every
// host.
type Allowlist []string

// Match reports whether host is allowed and whether it matched an entry
// exactly rather than through a wildcard.
func (a Allowlist) Match(host string) (allowed, exact bool) {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, pat := range a {
		pat = strings.ToLower(pat)
		switch {
		case pat == "*":
			allowed = true
		case strings.HasPrefix(pat, "*."):
			if strings.HasSuffix(host, pat[1:]) {
				allowed = true
			}
		case pat == host:
			return true, true
		}
	}
	return allowed, false
}

// Server is an HTTP forward proxy (CONNECT and absolute-URI requests)
// restricted to Allow.
type Server struct {
	Allow Allowlist
	// Log receives denied and failed requests; nil discards them.
	Log *log.Logger
	// DialTimeout bounds connecting to a destination; zero means 30s.
	DialTimeout time.Duration

	once      sync.Once
	transport *http.Transport
}

// Serve accepts proxy connections on ln until it is closed.
func (s *Server) Serve(ln net.Listener) error {
	srv := &http.Server{Handler: s, ReadHeaderTimeout: 30 * time.Second}
	err := srv.Serve(ln)
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func (s *Server) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log.Printf(format, args...)
	}
}

func (s *Server) init() {
	s.once.Do(func() {
		s.transport = &http.Transport{
			Proxy:                 nil,
			DialContext:           s.dial,
			ForceAttemptHTTP2:     false,
			MaxIdleConns:          32,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: 2 * time.Minute,
		}
	})
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.init()
	if r.Method == http.MethodConnect {
		s.connect(w, r)
		return
	}
	if !r.URL.IsAbs() {
		http.Error(w, "box proxy: absolute URL required", http.StatusBadRequest)
		return
	}
	if !s.allow(w, r.Method, r.URL.Host, r.URL.String()) {
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.Close = false
	dropHopHeaders(out.Header)
	resp, err := s.transport.RoundTrip(out)
	if err != nil {
		s.logf("%s %s: %v", r.Method, r.URL, err)
		http.Error(w, "box proxy: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	dropHopHeaders(resp.Header)
	for k, vs := range resp.Header {
		w.Header()[k] = vs
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// allow checks hostport against the allowlist, answering 403 and returning
// false when it is not allowed.
func (s *Server) allow(w http.ResponseWriter, method, hostport, what string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	if ok, _ := s.Allow.Match(host); ok {
		return true
	}
	s.logf("denied %s %s", method, what)
	http.Error(w, fmt.Sprintf("box proxy: host %q is not on the allowlist", host), http.StatusForbidden)
	return false
}

func (s *Server) connect(w http.ResponseWriter, r *http.Request) {
	if !s.allow(w, r.Method, r.Host, r.Host) {
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "box proxy: cannot hijack connection", http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.dialTimeout())
	upstream, err := s.dial(ctx, "tcp", r.Host)
	cancel()
	if err != nil {
		s.logf("CONNECT %s: %v", r.Host, err)
		http.Error(w, "box proxy: "+err.Error(), http.StatusBadGateway)
		return
	}
	client, brw, err := hj.Hijack()
	if err != nil {
		_ = upstream.Close()
		s.logf("CONNECT %s: hijack: %v", r.Host, err)
		return
	}
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		_ = client.Close()
		_ = upstream.Close()
		return
	}
	// brw may hold bytes the client sent right after the CONNECT header.
	splice(client, upstream, brw.Reader)
}

func (s *Server) dialTimeout() time.Duration {
	if s.DialTimeout > 0 {
		return s.DialTimeout
	}
	return 30 * time.Second
}

// dial connects to hostport, resolving the name itself so that a wildcard
// allowlist entry cannot be turned against the host: names that match only
// through a wildcard must not resolve to loopback or link-local addresses.
func (s *Server) dial(ctx context.Context, network, hostport string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return nil, err
	}
	allowed, exact := s.Allow.Match(host)
	if !allowed {
		return nil, fmt.Errorf("host %q is not on the allowlist", host)
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: s.dialTimeout()}
	var last error
	for _, a := range addrs {
		if !exact && (a.IP.IsLoopback() || a.IP.IsLinkLocalUnicast() || a.IP.IsUnspecified()) {
			last = fmt.Errorf("%s resolves to local address %s; list it explicitly to allow that", host, a.IP)
			continue
		}
		c, err := d.DialContext(ctx, network, net.JoinHostPort(a.IP.String(), port))
		if err == nil {
			return c, nil
		}
		last = err
	}
	if last == nil {
		last = fmt.Errorf("%s: no addresses", host)
	}
	return nil, last
}

var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func dropHopHeaders(h http.Header) {
	for _, f := range strings.Split(h.Get("Connection"), ",") {
		if f = strings.TrimSpace(f); f != "" {
			h.Del(f)
		}
	}
	for _, k := range hopHeaders {
		h.Del(k)
	}
}

// Forward accepts connections on ln and relays each to the unix socket at
// path until ln is closed.
func Forward(ln net.Listener, path string) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go func() {
			up, err := net.DialTimeout("unix", path, 10*time.Second)
			if err != nil {
				_ = c.Close()
				return
			}
			splice(c, up, c)
		}()
	}
}

// splice copies between a and b in both directions, where ar is the reader
// to use for a (a buffered wrapper, or a itself), and closes both when
// either side finishes.
func splice(a, b net.Conn, ar io.Reader) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(b, ar)
		closeWrite(b)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(a, b)
		closeWrite(a)
	}()
	wg.Wait()
	_ = a.Close()
	_ = b.Close()
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// ReadyFD is the descriptor a `box _proxy` process inherits to report that
// it is listening: it writes one byte to it and closes it.
const ReadyFD = 3
