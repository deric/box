package main

import "testing"

func TestCwdRefusal(t *testing.T) {
	for _, tc := range []struct{ cwd, home, want string }{
		{"/", "/home/u", "it is the root directory"},
		{"/", "", "it is the root directory"},
		{"//", "/home/u", "it is the root directory"},
		{"/home/u", "/home/u", "it is the home directory"},
		{"/home/u/", "/home/u/", "it is the home directory"},
		{"/home", "/home/u", "it contains the home directory /home/u"},
		{"/Users", "/Users/u/", "it contains the home directory /Users/u"},
		{"/home/u/dev", "/home/u", ""},
		{"/home/user2", "/home/u", ""},
		{"/home/us", "/home/user", ""},
		{"/srv", "/home/u", ""},
		{"/home", "", ""},
	} {
		if got := cwdRefusal(tc.cwd, tc.home); got != tc.want {
			t.Errorf("cwdRefusal(%q, %q) = %q, want %q", tc.cwd, tc.home, got, tc.want)
		}
	}
}
