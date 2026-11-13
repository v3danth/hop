package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRegistryRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	reg, err := loadRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	mustPut(t, reg, Service{Name: "yt", URL: "https://youtube.com"})
	reg.SaveIfDirty()
	loaded, err := loadRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if svc, ok := loaded.Lookup("yt"); !ok || svc.URL != "https://youtube.com" {
		t.Errorf("Lookup(yt) = %+v, %v", svc, ok)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("registry mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
}

func TestLoadRegistrySkipsInvalidEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	// A tampered file must not smuggle extra lines into /etc/hosts.
	data := `[{"name":"ok","url":"https://ok.test"},
	          {"name":"x\n1.2.3.4 bank.example","url":"https://evil.test"},
	          {"name":"hop","port":80}]`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := loadRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if names := reg.Names(); len(names) != 1 || names[0] != "ok" {
		t.Errorf("names = %q, want only ok", names)
	}
}

func TestUndoRemoveKeepsHistory(t *testing.T) {
	reg := newTestRegistry(t)
	mustPut(t, reg, Service{Name: "yt", URL: "https://youtube.com"})
	reg.RecordHit("yt")
	reg.RecordHit("yt")
	reg.Remove("yt", 0)
	restored, err := reg.Put(Service{Name: "yt", URL: "https://youtube.com"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Hits != 2 || restored.LastHit.IsZero() {
		t.Errorf("restored = %+v, want 2 hits and a last visit", restored)
	}
	if other, _ := reg.Put(Service{Name: "gh", URL: "https://github.com"}, false); other.Hits != 0 {
		t.Errorf("unrelated name inherited history: %+v", other)
	}
}

func TestPutWithoutReplaceKeepsExistingName(t *testing.T) {
	reg := newTestRegistry(t)
	mustPut(t, reg, Service{Name: "yt", URL: "https://github.com"})
	if _, err := reg.Put(Service{Name: "yt", URL: "https://github.com"}, false); err != nil {
		t.Errorf("re-adding the same target: %v, want no error", err)
	}
	_, err := reg.Put(Service{Name: "yt", URL: "https://youtube.com"}, false)
	var taken *NameTakenError
	if !errors.As(err, &taken) || err.Error() != "yt/ already goes to https://github.com" {
		t.Errorf("Put over a different target: %v, want NameTakenError", err)
	}
	if svc, _ := reg.Lookup("yt"); svc.URL != "https://github.com" {
		t.Errorf("yt now goes to %s, want it unchanged", svc.URL)
	}
}

func TestPutWithoutReplaceComparesOwner(t *testing.T) {
	reg := newTestRegistry(t)
	mustPut(t, reg, Service{Name: "web", Port: 5173, PID: 200})
	// Making a running app's name permanent must not silently keep it
	// tied to the app's process.
	if _, err := reg.Put(Service{Name: "web", Port: 5173}, false); err == nil {
		t.Error("permanent add over an app's name with the same port succeeded without replacing")
	}
	if svc, err := reg.Put(Service{Name: "web", Port: 5173}, true); err != nil || svc.PID != 0 {
		t.Errorf("replace = %+v, %v; want a permanent entry", svc, err)
	}
}

func TestRemoveWithPIDOnlyRemovesOwnName(t *testing.T) {
	reg := newTestRegistry(t)
	mustPut(t, reg, Service{Name: "web", Port: 5173, PID: 200})
	if reg.Remove("web", 100) {
		t.Error("an earlier run (pid 100) removed the name pid 200 holds")
	}
	if !reg.Remove("web", 200) {
		t.Error("the holder (pid 200) could not remove its name")
	}
}

func TestValidateService(t *testing.T) {
	tests := []struct {
		svc     Service
		wantErr bool
	}{
		{Service{Name: "yt", URL: "https://youtube.com"}, false},
		{Service{Name: "ys", URL: "https://youtube.com/results?search_query=%s"}, false},
		{Service{Name: "api", Port: 8080}, false},
		{Service{Name: "web3", Port: 8080}, false},
		{Service{Name: "0xide", Port: 8080}, false}, // not valid hex, so a name
		{Service{Name: "123", Port: 8080}, true},    // browsers open 0.0.0.123
		{Service{Name: "0x1f", Port: 8080}, true},
		{Service{Name: "Bad", Port: 8080}, true},
		{Service{Name: "-x", Port: 8080}, true},
		{Service{Name: "hop", Port: 8080}, true},
		{Service{Name: "x", Port: 70000}, true},
		{Service{Name: "x", Port: -1}, true},
		{Service{Name: "x", URL: "ftp://a"}, true},
		{Service{Name: "x", URL: "https://a", Port: 1}, true},
		{Service{Name: "x"}, true},
	}
	for _, tt := range tests {
		if err := validateService(tt.svc); (err != nil) != tt.wantErr {
			t.Errorf("validateService(%+v) error = %v, wantErr %v", tt.svc, err, tt.wantErr)
		}
	}
}

func TestWriteFileAtomicCleansUpOnFailure(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "taken")
	// A directory in the way makes the final rename fail.
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "keep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(target, []byte("x"), 0o644); err == nil {
		t.Fatal("writeFileAtomic over a non-empty directory succeeded")
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, ".taken.tmp-*")); len(leftovers) > 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
}
