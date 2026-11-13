package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Service is one name the proxy answers for: either a redirect to a website
// or a local app listening on a loopback port.
type Service struct {
	Name    string    `json:"name"`
	URL     string    `json:"url,omitempty"`
	Port    int       `json:"port,omitempty"`
	PID     int       `json:"pid,omitempty"`
	Created time.Time `json:"created"`
	Hits    int64     `json:"hits"`
	LastHit time.Time `json:"lastHit,omitzero"`
}

func (s Service) isLocal() bool { return s.Port > 0 }

// target is where the name goes, as people read it.
func (s Service) target() string {
	if s.isLocal() {
		return "localhost:" + strconv.Itoa(s.Port)
	}
	return s.URL
}

// NameTakenError stops a typo or a reused name from silently replacing a
// shortcut someone relies on.
type NameTakenError struct{ Existing Service }

func (e *NameTakenError) Error() string {
	return e.Existing.Name + "/ already goes to " + e.Existing.target()
}

// Names must be valid DNS labels so they work as hosts-file entries.
var namePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// The server itself answers on these names; registering one would hide the
// dashboard.
var reservedNames = []string{"go", "hop", "localhost"}

func validateService(s Service) error {
	if !namePattern.MatchString(s.Name) {
		return fmt.Errorf("name %q can only use lowercase letters, digits and dashes", s.Name)
	}
	if slices.Contains(reservedNames, s.Name) {
		return fmt.Errorf("name %q is reserved for hop itself", s.Name)
	}
	switch {
	case s.URL != "" && s.Port != 0:
		return errors.New("give either a url or a port, not both")
	case s.Port != 0:
		if s.Port < 1 || s.Port > 65535 {
			return fmt.Errorf("port %d is out of range 1-65535", s.Port)
		}
	case s.URL != "":
		// %s marks where a search term goes; it is not a valid escape for url.Parse.
		u, err := url.Parse(strings.ReplaceAll(s.URL, "%s", "x"))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("url %q must start with http:// or https://", s.URL)
		}
	default:
		return errors.New("give a url or a port")
	}
	return nil
}

// Registry is the name table, persisted as JSON so links survive restarts.
type Registry struct {
	path    string
	mu      sync.Mutex
	byName  map[string]Service
	dirty   bool
	changed chan struct{} // signals the maintenance loop that names changed
	// lastRemoved lets the dashboard's Undo restore a name with its history.
	lastRemoved Service
}

func loadRegistry(path string) (*Registry, error) {
	r := &Registry{path: path, byName: map[string]Service{}, changed: make(chan struct{}, 1)}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read registry: %w", err)
	}
	var list []Service
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("parse registry %s: %w", path, err)
	}
	for _, s := range list {
		r.byName[s.Name] = s
	}
	return r, nil
}

// markChanged must be called with r.mu held.
func (r *Registry) markChanged() {
	r.dirty = true
	select {
	case r.changed <- struct{}{}:
	default:
	}
}

// Put adds or replaces a service. Visit counts carry over so re-registering an
// app on a new port, or undoing the last removal, keeps its history.
func (r *Registry) Put(s Service) (Service, error) {
	if err := validateService(s); err != nil {
		return Service{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s.Created = time.Now()
	if old, ok := r.historyOf(s.Name); ok {
		s.Created, s.Hits, s.LastHit = old.Created, old.Hits, old.LastHit
	}
	r.byName[s.Name] = s
	r.markChanged()
	return s, nil
}

// Add registers s only if the name is free. Adding the same name with the
// same target again is a no-op, so scripts can run it repeatedly.
func (r *Registry) Add(s Service) (Service, error) {
	if err := validateService(s); err != nil {
		return Service{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.byName[s.Name]; ok {
		if old.target() == s.target() {
			return old, nil
		}
		return Service{}, &NameTakenError{Existing: old}
	}
	s.Created = time.Now()
	if old, ok := r.historyOf(s.Name); ok {
		s.Created, s.Hits, s.LastHit = old.Created, old.Hits, old.LastHit
	}
	r.byName[s.Name] = s
	r.markChanged()
	return s, nil
}

// historyOf must be called with r.mu held.
func (r *Registry) historyOf(name string) (Service, bool) {
	if s, ok := r.byName[name]; ok {
		return s, true
	}
	return r.lastRemoved, r.lastRemoved.Name == name
}

func (r *Registry) Remove(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.byName[name]
	if !ok {
		return false
	}
	r.lastRemoved = s
	delete(r.byName, name)
	r.markChanged()
	return true
}

func (r *Registry) Lookup(name string) (Service, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.byName[name]
	return s, ok
}

// Visits are flushed to disk by the maintenance loop, not on every hit.
func (r *Registry) RecordHit(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.byName[name]
	if !ok {
		return
	}
	s.Hits++
	s.LastHit = time.Now()
	r.byName[name] = s
	r.dirty = true
}

func (r *Registry) List() []Service {
	r.mu.Lock()
	list := make([]Service, 0, len(r.byName))
	for _, s := range r.byName {
		list = append(list, s)
	}
	r.mu.Unlock()
	slices.SortFunc(list, func(a, b Service) int { return strings.Compare(a.Name, b.Name) })
	return list
}

func (r *Registry) Names() []string {
	list := r.List()
	names := make([]string, len(list))
	for i, s := range list {
		names[i] = s.Name
	}
	return names
}

// ReapDead drops services whose registering process has exited, so a crashed
// app does not leave a stale name behind. Services without a PID are permanent.
func (r *Registry) ReapDead() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var dead []string
	for name, s := range r.byName {
		if s.PID > 0 && !isProcessAlive(s.PID) {
			dead = append(dead, name)
			delete(r.byName, name)
		}
	}
	if len(dead) > 0 {
		r.markChanged()
	}
	return dead
}

func isProcessAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func (r *Registry) SaveIfDirty() {
	r.mu.Lock()
	if !r.dirty {
		r.mu.Unlock()
		return
	}
	r.dirty = false
	r.mu.Unlock()
	if err := r.save(); err != nil {
		slog.Error("save registry failed", "path", r.path, "err", err)
		r.mu.Lock()
		r.dirty = true
		r.mu.Unlock()
	}
}

func (r *Registry) save() error {
	b, err := json.MarshalIndent(r.List(), "", "  ")
	if err != nil {
		return fmt.Errorf("encode registry: %w", err)
	}
	dir := filepath.Dir(r.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	return writeFileAtomic(r.path, b, 0o644)
}

// A crash mid-write must never leave a half-written registry or hosts file,
// so write a temp file next to path and rename it over the old one. The
// daemon runs as root, so the file takes the owner of its directory;
// otherwise root would own the registry in the user's home.
func writeFileAtomic(path string, b []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := writeAndClose(tmp, b, perm, dir); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

func writeAndClose(f *os.File, b []byte, perm os.FileMode, ownerDir string) error {
	if err := prepareTemp(f, b, perm, ownerDir); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	return nil
}

func prepareTemp(f *os.File, b []byte, perm os.FileMode, ownerDir string) error {
	if _, err := f.Write(b); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := f.Chmod(perm); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if os.Geteuid() == 0 {
		uid, gid, err := ownerOf(ownerDir)
		if err != nil {
			return err
		}
		if err := f.Chown(uid, gid); err != nil {
			return fmt.Errorf("chown temp file: %w", err)
		}
	}
	// Without a sync, a power cut after the rename can leave an empty file.
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}
	return nil
}

func ownerOf(path string) (uid, gid int, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, fmt.Errorf("stat %s: %w", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("stat %s: no owner information", path)
	}
	return int(st.Uid), int(st.Gid), nil
}
