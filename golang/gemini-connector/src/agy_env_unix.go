//go:build !windows

package main

import (
	"os"
	"path/filepath"
)

// findGrepDir checks the conventional tool directories for grep when it is
// missing from PATH, covering minimal containers and GUI-launched processes
// whose inherited environment dropped /usr/bin or /bin.
func findGrepDir() (string, bool) {
	for _, dir := range []string{"/usr/bin", "/bin", "/usr/local/bin"} {
		info, err := os.Stat(filepath.Join(dir, "grep"))
		if err == nil && !info.IsDir() {
			return dir, true
		}
	}
	return "", false
}
