package diff

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnifiedEqual(t *testing.T) {
	if d := Unified("a", "b", []byte("x\ny\n"), []byte("x\ny\n")); d != "" {
		t.Errorf("equal texts: got %q", d)
	}
	if d := Unified("a", "b", nil, nil); d != "" {
		t.Errorf("empty texts: got %q", d)
	}
}

func TestUnified(t *testing.T) {
	a := "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\nten\n"
	b := "one\ntwo\nthree\nFOUR\nfive\nsix\nseven\neight\nnine\nten\neleven\n"
	want := `--- host
+++ sandbox
@@ -1,10 +1,11 @@
 one
 two
 three
-four
+FOUR
 five
 six
 seven
 eight
 nine
 ten
+eleven
`
	if got := Unified("host", "sandbox", []byte(a), []byte(b)); got != want {
		t.Errorf("Unified:\n%s\nwant:\n%s", got, want)
	}
}

func TestUnifiedSeparateHunks(t *testing.T) {
	var sb strings.Builder
	for i := 1; i <= 20; i++ {
		sb.WriteString(strings.Repeat("l", i) + "\n")
	}
	a := sb.String()
	b := strings.Replace(strings.Replace(a, "l\n", "L\n", 1), "llllllllllllllllllll\n", "", 1)
	got := Unified("a", "b", []byte(a), []byte(b))
	if strings.Count(got, "@@ -") != 2 {
		t.Errorf("want two hunks:\n%s", got)
	}
	if !strings.Contains(got, "@@ -1,4 +1,4 @@\n-l\n+L\n ll\n lll\n llll\n") {
		t.Errorf("first hunk:\n%s", got)
	}
	if !strings.HasSuffix(got, "@@ -17,4 +17,3 @@\n lllllllllllllllll\n llllllllllllllllll\n lllllllllllllllllll\n-llllllllllllllllllll\n") {
		t.Errorf("last hunk:\n%s", got)
	}
}

func TestUnifiedNoTrailingNewline(t *testing.T) {
	got := Unified("a", "b", []byte("x\ny"), []byte("x\ny\n"))
	want := "--- a\n+++ b\n@@ -1,2 +1,2 @@\n x\n-y\n\\ No newline at end of file\n+y\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestUnifiedFromEmpty(t *testing.T) {
	got := Unified("a", "b", nil, []byte("x\n"))
	want := "--- a\n+++ b\n@@ -0,0 +1 @@\n+x\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// TestUnifiedMatchesDiff compares the output with diff -u where available.
func TestUnifiedMatchesDiff(t *testing.T) {
	bin, err := exec.LookPath("diff")
	if err != nil {
		t.Skip("diff not installed")
	}
	dir := t.TempDir()
	cases := [][2]string{
		{"a\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\n", "a\nb\nc\nd\nE\nf\ng\nh\ni\nj\nk\nl\n"},
		{"a\nb\nc\n", "c\nb\na\n"},
		{"", "a\n"},
		{"a\nb\n", ""},
		{"x\n" + strings.Repeat("same\n", 10) + "y\n", "z\n" + strings.Repeat("same\n", 10) + "w\n"},
		{strings.Repeat("same\n", 3) + "y\n" + strings.Repeat("same\n", 7), strings.Repeat("same\n", 3) + strings.Repeat("same\n", 7) + "end\n"},
	}
	for _, c := range cases {
		pa, pb := filepath.Join(dir, "a"), filepath.Join(dir, "b")
		for p, s := range map[string]string{pa: c[0], pb: c[1]} {
			if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		out, _ := exec.Command(bin, "-u", "--label", "a", "--label", "b", pa, pb).Output()
		got := Unified("a", "b", []byte(c[0]), []byte(c[1]))
		if got != string(out) {
			t.Errorf("%q vs %q:\n got:\n%s\nwant:\n%s", c[0], c[1], got, out)
		}
	}
}
