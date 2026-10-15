package sandbox

import (
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// TestPrepareDNS runs itself again under unshare(1), the way `box run`
// starts `box _netns`, and there checks that PrepareDNS binds the
// listeners and leaves no capability behind. It is skipped where user
// namespaces cannot be created.
func TestPrepareDNS(t *testing.T) {
	if os.Getenv("BOX_TEST_NETNS") == "1" {
		// Capabilities are per thread; stay on the one PrepareDNS acts on.
		runtime.LockOSThread()
		if err := PrepareDNS(-1, -1); err != nil {
			t.Fatalf("PrepareDNS: %v", err)
		}
		status, err := os.ReadFile("/proc/thread-self/status")
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, "Cap") && !strings.HasSuffix(line, "0000000000000000") {
				t.Errorf("capability left behind: %s", line)
			}
		}
		// Without capabilities the privileged port is out of reach again.
		if ln, err := net.Listen("tcp4", DNSAddr); err == nil {
			_ = ln.Close()
			t.Errorf("listening on %s still allowed after dropping capabilities", DNSAddr)
		}
		return
	}
	unshare, err := lookupUnshare()
	if err != nil {
		t.Skip("unshare not found")
	}
	args := append(unshareArgs(os.Getuid(), os.Getgid()), os.Args[0], "-test.run=^TestPrepareDNS$", "-test.v")
	cmd := exec.Command(unshare, args...)
	cmd.Env = append(os.Environ(), "BOX_TEST_NETNS=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "unshare failed") || strings.Contains(string(out), "unrecognized option") {
			t.Skipf("cannot create the namespaces here: %s", strings.TrimSpace(string(out)))
		}
		t.Fatalf("%v:\n%s", err, out)
	}
	if !strings.Contains(string(out), "--- PASS: TestPrepareDNS") {
		t.Errorf("unexpected output:\n%s", out)
	}
}
