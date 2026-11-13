package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	hostsBegin = "# hop begin (managed by hop, edits here are overwritten)"
	hostsEnd   = "# hop end"
)

// hostsWithNames returns the hosts file with hop's block replaced. Browsers
// resolve name.localhost on their own, but single-word names like yt/ and
// tools such as curl or Safari need a real hosts entry.
func hostsWithNames(current, ip string, names []string) string {
	var out strings.Builder
	out.Grow(len(current) + len(names)*48)
	inBlock := false
	for line := range strings.Lines(current) {
		switch strings.TrimSpace(line) {
		case hostsBegin:
			inBlock = true
		case hostsEnd:
			inBlock = false
		default:
			if !inBlock {
				out.WriteString(line)
			}
		}
	}
	kept := strings.TrimRight(out.String(), "\n")
	out.Reset()
	if kept != "" {
		out.WriteString(kept)
		out.WriteString("\n\n")
	}
	out.WriteString(hostsBegin + "\n")
	for _, n := range names {
		out.WriteString(ip + " " + n + " " + n + ".localhost\n")
	}
	out.WriteString(hostsEnd + "\n")
	return out.String()
}

func syncHosts(path, ip string, names []string) error {
	// Replace the real file, not a symlink pointing at it.
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", path, err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return fmt.Errorf("stat %s: %w", real, err)
	}
	b, err := os.ReadFile(real)
	if err != nil {
		return fmt.Errorf("read %s: %w", real, err)
	}
	want := hostsWithNames(string(b), ip, append([]string{"go", "hop"}, names...))
	if want == string(b) {
		return nil
	}
	// A truncated /etc/hosts breaks name resolution for the whole machine.
	if err := writeFileAtomic(real, []byte(want), info.Mode().Perm()); err != nil {
		return fmt.Errorf("write %s: %w", real, err)
	}
	if path == "/etc/hosts" {
		flushDNSCache()
	}
	return nil
}

// macOS caches lookups, so a new name would not resolve until the cache
// expires. Failures only delay resolution, so they are logged, not returned.
func flushDNSCache() {
	for _, args := range [][]string{{"dscacheutil", "-flushcache"}, {"killall", "-HUP", "mDNSResponder"}} {
		if err := exec.Command(args[0], args[1:]...).Run(); err != nil {
			slog.Warn("dns cache flush failed", "cmd", args[0], "err", err)
		}
	}
}
