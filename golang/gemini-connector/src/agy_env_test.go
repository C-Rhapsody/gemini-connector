package main

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestPathListSeparator(t *testing.T) {
	if runtime.GOOS == "windows" {
		if pathListSeparator() != ";" {
			t.Fatalf("windows separator = %q, want %q", pathListSeparator(), ";")
		}
		return
	}
	if pathListSeparator() != ":" {
		t.Fatalf("unix separator = %q, want %q", pathListSeparator(), ":")
	}
}

func TestPathWithEntry(t *testing.T) {
	sep := pathListSeparator()
	if got := pathWithEntry("/tools/bin", ""); got != "/tools/bin" {
		t.Fatalf("empty PATH handling failed: %q", got)
	}
	got := pathWithEntry("C:\\Git\\usr\\bin", "C:\\Windows;C:\\Bin")
	want := "C:\\Git\\usr\\bin" + sep + "C:\\Windows;C:\\Bin"
	if runtime.GOOS == "windows" && got != want {
		t.Fatalf("prepended entry misplaced: %q", got)
	}
	if !strings.HasPrefix(got, "C:\\Git\\usr\\bin"+sep) {
		t.Fatalf("located directory must come first: %q", got)
	}
}

func TestTrimPathList(t *testing.T) {
	sep := pathListSeparator()
	cases := []struct{ in, want string }{
		{"  /usr/bin  ", "/usr/bin"},
		{"/usr/bin" + sep, "/usr/bin"},
		{" /x/y " + sep + " ", "/x/y"},
		{"", ""},
	}
	for _, c := range cases {
		if got := trimPathList(c.in); got != c.want {
			t.Fatalf("trimPathList(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestAgyEnvKeepsEnvironment(t *testing.T) {
	env := agyEnv()
	if len(env) < len(os.Environ()) {
		t.Fatalf("agyEnv dropped entries: got %d, base %d", len(env), len(os.Environ()))
	}
	found := false
	for _, e := range env {
		if strings.HasPrefix(strings.ToUpper(e), "PATH=") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("agyEnv must always carry a PATH entry")
	}
}
