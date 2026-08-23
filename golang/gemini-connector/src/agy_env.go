package main

import (
	"log"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
)

// agyEnv builds the child-process environment for agy invocations. agy's
// grep_search tool shells out to a literal "grep" executable, which is absent
// from the PATH of many Windows terminals (Git for Windows ships grep.exe in
// <git-root>\usr\bin while only <git-root>\cmd is normally on PATH) and from
// GUI-launched processes with stripped environments. When grep cannot be
// resolved, a located directory is prepended to the inherited PATH so the
// connector works from any terminal. The lookup runs once per process; the
// system PATH itself is never modified.
var agyEnv = sync.OnceValue(func() []string {
	env := os.Environ()
	if _, err := exec.LookPath("grep"); err == nil {
		return env
	}
	dir, ok := findGrepDir()
	if !ok {
		log.Println("grep not found on PATH or in known locations; agy grep_search may fail until grep (or Git on Windows) is installed and resolvable")
		return env
	}
	list := pathWithEntry(dir, os.Getenv("PATH"))
	log.Printf("grep not on PATH; prepending %q to the PATH of agy child processes", dir)
	return append(env, "PATH="+list)
})

// pathListSeparator returns the separator used between entries of a PATH
// value on the current platform.
func pathListSeparator() string {
	if runtime.GOOS == "windows" {
		return ";"
	}
	return ":"
}

// pathWithEntry prepends dir to a raw PATH value, tolerating an empty one.
func pathWithEntry(dir string, pathList string) string {
	if pathList == "" {
		return dir
	}
	return dir + pathListSeparator() + pathList
}

// trimPathList normalizes a single PATH entry: surrounding whitespace is
// removed and trailing separators are dropped so comparisons are stable.
func trimPathList(entry string) string {
	entry = strings.TrimSpace(entry)
	entry = strings.TrimRight(entry, pathListSeparator())
	return strings.TrimSpace(entry)
}
