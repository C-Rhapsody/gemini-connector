//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
)

// findGrepDir locates a directory containing grep.exe. Windows Git
// distributions ship one in <git-root>\usr\bin while only <git-root>\cmd (or
// <git-root>\bin, or mingw64\bin for portable layouts) is on PATH; the Git
// root is therefore derived from the resolved git.exe by walking upward, so
// standard, portable and user-scoped installations all work without hardcoding.
func findGrepDir() (string, bool) {
	git, err := exec.LookPath("git")
	if err != nil {
		return "", false
	}
	dir := filepath.Dir(git)
	for i := 0; i < 4 && dir != ""; i++ {
		candidate := filepath.Join(dir, "usr", "bin")
		if grepExistsIn(candidate) {
			return candidate, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", false
}

func grepExistsIn(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "grep.exe"))
	return err == nil && !info.IsDir()
}
