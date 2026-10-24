package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestListenProxy(t *testing.T) {
	ln, err := listenProxy("", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	if host, _, err := net.SplitHostPort(ln.Addr().String()); err != nil || host != "127.0.0.1" || ln.Addr().Network() != "tcp" {
		t.Errorf("tcp listener at %s %s", ln.Addr().Network(), ln.Addr())
	}

	sock := filepath.Join(t.TempDir(), "proxy.sock")
	if err := os.WriteFile(sock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	uln, err := listenProxy(sock, "")
	if err != nil {
		t.Fatalf("a stale socket file must be replaced: %v", err)
	}
	defer func() { _ = uln.Close() }()
	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode = %v, want a socket with 0600", fi.Mode())
	}
	if _, err := listenProxy("", ""); err == nil {
		t.Error("listening nowhere should fail")
	}
}
