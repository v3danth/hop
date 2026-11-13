package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostsWithNames(t *testing.T) {
	tests := []struct {
		name, current string
		names         []string
		want          string
	}{
		{
			name:    "adds block",
			current: "127.0.0.1 localhost\n",
			names:   []string{"yt"},
			want:    "127.0.0.1 localhost\n\n" + hostsBegin + "\n127.0.0.2 yt yt.localhost\n" + hostsEnd + "\n",
		},
		{
			name:    "replaces block and keeps lines after it",
			current: "a\n\n" + hostsBegin + "\n127.0.0.2 old old.localhost\n" + hostsEnd + "\nb\n",
			names:   []string{"new"},
			want:    "a\n\nb\n\n" + hostsBegin + "\n127.0.0.2 new new.localhost\n" + hostsEnd + "\n",
		},
		{
			name:    "empty file",
			current: "",
			names:   nil,
			want:    hostsBegin + "\n" + hostsEnd + "\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hostsWithNames(tt.current, "127.0.0.2", tt.names); got != tt.want {
				t.Errorf("hostsWithNames() =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

func TestHostsWithNamesIsIdempotent(t *testing.T) {
	once := hostsWithNames("127.0.0.1 localhost\n", "127.0.0.2", []string{"a", "b"})
	if twice := hostsWithNames(once, "127.0.0.2", []string{"a", "b"}); twice != once {
		t.Errorf("second sync changed the file:\n%q\nwant\n%q", twice, once)
	}
}

func TestSyncHostsKeepsModeAndContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(path, []byte("127.0.0.1 localhost\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := syncHosts(path, "127.0.0.2", []string{"yt"}); err != nil {
		t.Fatalf("syncHosts: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "127.0.0.2 yt yt.localhost\n") || !strings.HasPrefix(string(b), "127.0.0.1 localhost\n") {
		t.Errorf("hosts file = %q", b)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", info.Mode().Perm())
	}
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".hosts.tmp-*")); len(leftovers) > 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
}

func TestSyncHostsReplacesSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real-hosts")
	link := filepath.Join(dir, "hosts")
	if err := os.WriteFile(real, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := syncHosts(link, "127.0.0.2", []string{"yt"}); err != nil {
		t.Fatalf("syncHosts: %v", err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("symlink was replaced by a regular file")
	}
}

func TestHostsSyncerRestoresBlockAfterOutsideEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(path, []byte("127.0.0.1 localhost\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := &hostsSyncer{path: path, ip: "127.0.0.2"}
	h.sync([]string{"yt"})
	// Something else, like a system update, rewrites the file without hop's block.
	if err := os.WriteFile(path, []byte("127.0.0.1 localhost\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.sync([]string{"yt"})
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "127.0.0.2 yt yt.localhost") {
		t.Errorf("block not restored: %q", b)
	}
	if h.lastErr != "" {
		t.Errorf("lastErr = %q, want none", h.lastErr)
	}
}

func TestHostsSyncerRemembersFailure(t *testing.T) {
	h := &hostsSyncer{path: filepath.Join(t.TempDir(), "missing", "hosts"), ip: "127.0.0.2"}
	h.sync(nil)
	if h.lastErr == "" {
		t.Error("a failed sync left no error to compare the next attempt against")
	}
}
