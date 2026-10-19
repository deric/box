// Package diff renders the unified diff of two texts, as diff -u prints it.
package diff

import (
	"fmt"
	"strings"
)

// Unified returns the unified diff of a and b with three lines of context,
// headed by the two names. It is empty when the texts are equal.
func Unified(nameA, nameB string, a, b []byte) string {
	al, bl := lines(a), lines(b)
	ops := myers(al, bl)
	hunks := group(ops, 3)
	if len(hunks) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", nameA, nameB)
	for _, h := range hunks {
		fmt.Fprintf(&sb, "@@ -%s +%s @@\n", span(h.aStart, h.aLen), span(h.bStart, h.bLen))
		for _, o := range h.ops {
			switch o.kind {
			case keep:
				sb.WriteString(" " + al[o.a])
			case del:
				sb.WriteString("-" + al[o.a])
			case ins:
				sb.WriteString("+" + bl[o.b])
			}
		}
	}
	return sb.String()
}

// span renders a hunk range as diff does: "start,len", "start" when len is
// one, "start-1,0" for an empty range.
func span(start, n int) string {
	switch n {
	case 0:
		return fmt.Sprintf("%d,0", start)
	case 1:
		return fmt.Sprintf("%d", start+1)
	}
	return fmt.Sprintf("%d,%d", start+1, n)
}

// lines splits text into lines keeping their newlines; a last line without
// one is marked the way diff does.
func lines(text []byte) []string {
	if len(text) == 0 {
		return nil
	}
	s := string(text)
	parts := strings.SplitAfter(s, "\n")
	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	} else {
		parts[len(parts)-1] += "\n\\ No newline at end of file\n"
	}
	return parts
}

type opKind int

const (
	keep opKind = iota
	del
	ins
)

// op is one step of an edit script: a kept or deleted line a of the first
// text, or an inserted line b of the second.
type op struct {
	kind opKind
	a, b int
}

// myers computes the shortest edit script from a to b (Myers, "An O(ND)
// Difference Algorithm and Its Variations", the forward version with a
// trace for backtracking).
func myers(a, b []string) []op {
	n, m := len(a), len(b)
	max := n + m
	if max == 0 {
		return nil
	}
	// v[k+max] is the furthest x reached on diagonal k.
	v := make([]int, 2*max+2)
	var trace [][]int
	found := false
	for d := 0; d <= max && !found; d++ {
		snapshot := make([]int, len(v))
		copy(snapshot, v)
		trace = append(trace, snapshot)
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[k-1+max] < v[k+1+max]) {
				x = v[k+1+max]
			} else {
				x = v[k-1+max] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[k+max] = x
			if x >= n && y >= m {
				found = true
				break
			}
		}
	}
	// Walk the trace back from (n, m) to (0, 0).
	var rev []op
	x, y := n, m
	for d := len(trace) - 1; d >= 0; d-- {
		v := trace[d]
		k := x - y
		var prevK int
		if k == -d || (k != d && v[k-1+max] < v[k+1+max]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := v[prevK+max]
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			x--
			y--
			rev = append(rev, op{keep, x, y})
		}
		if d > 0 {
			if x == prevX {
				y--
				rev = append(rev, op{ins, x, y})
			} else {
				x--
				rev = append(rev, op{del, x, y})
			}
		}
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}

type hunk struct {
	aStart, aLen, bStart, bLen int
	ops                        []op
}

// group splits an edit script into hunks of changes surrounded by at most
// ctx lines of context, merging hunks whose context would touch.
func group(ops []op, ctx int) []hunk {
	var hunks []hunk
	i := 0
	for i < len(ops) {
		if ops[i].kind == keep {
			i++
			continue
		}
		start := i - ctx
		if start < 0 {
			start = 0
		}
		end := i
		// Extend past the last change while the gap to the next change is
		// within 2*ctx kept lines.
		for end < len(ops) {
			if ops[end].kind != keep {
				end++
				continue
			}
			j := end
			for j < len(ops) && ops[j].kind == keep {
				j++
			}
			if j == len(ops) || j-end > 2*ctx {
				end += min(j-end, ctx)
				break
			}
			end = j
		}
		h := hunk{ops: ops[start:end]}
		h.aStart, h.bStart = pos(ops, start)
		for _, o := range h.ops {
			switch o.kind {
			case keep:
				h.aLen++
				h.bLen++
			case del:
				h.aLen++
			case ins:
				h.bLen++
			}
		}
		hunks = append(hunks, h)
		i = end
	}
	return hunks
}

// pos returns the line positions in a and b at which ops[i] applies.
func pos(ops []op, i int) (a, b int) {
	if i >= len(ops) {
		for _, o := range ops {
			switch o.kind {
			case keep:
				a++
				b++
			case del:
				a++
			case ins:
				b++
			}
		}
		return a, b
	}
	return ops[i].a, ops[i].b
}
