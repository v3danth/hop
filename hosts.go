package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	hostsBegin = "# hop begin (managed by hop, edits here are overwritten)"
	hostsEnd   = "# hop end"
)

// hostsWithNames returns the hosts file with hop's block replaced. Browsers
// resolve name.localhost on their own, but single-word names like yt/ and
// tools such as curl need a real hosts entry.
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

// syncHosts makes the hosts file at path list names, plus hop's own, at ip.
// It leaves the file untouched when nothing changed.
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
		announceHostsChange(real)
	}
	return nil
}

// announceHostsChange makes the system pick up a rewritten /etc/hosts.
// Failures only delay or degrade resolution, so they are logged, not returned.
func announceHostsChange(path string) {
	switch runtime.GOOS {
	case "darwin":
		// mDNSResponder caches lookups, so new names would not resolve
		// until the cache expired.
		runLogged("dscacheutil", "-flushcache")
		runLogged("killall", "-HUP", "mDNSResponder")
	case "linux":
		// glibc and systemd-resolved re-read the file by themselves. Under
		// SELinux, though, the replacement got its directory's generic label
		// rather than the one for hosts files, which confined services may
		// not be allowed to read.
		if _, err := os.Stat("/sys/fs/selinux/enforce"); err == nil {
			runLogged("restorecon", path)
		}
	}
}

func runLogged(name string, args ...string) {
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		slog.Warn("command failed", "cmd", name, "err", err, "output", strings.TrimSpace(string(out)))
	}
}

// hostsSyncer keeps hop's block in a hosts file current. It is retried every
// few seconds, so it logs a failure once rather than on every retry.
type hostsSyncer struct {
	path, ip string
	lastErr  string
}

func (h *hostsSyncer) sync(names []string) {
	if h.path == "" {
		return
	}
	err := syncHosts(h.path, h.ip, names)
	switch {
	case err != nil && err.Error() != h.lastErr:
		slog.Error("hosts sync failed; retrying every few seconds", "err", err)
		h.lastErr = err.Error()
	case err == nil && h.lastErr != "":
		slog.Info("hosts sync working again", "path", h.path)
		h.lastErr = ""
	}
}
