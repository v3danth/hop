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

var (
	// Names must be valid DNS labels so they work as hosts-file entries.
	namePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	// Browsers read these as IPv4 addresses, not names: http://123/ opens
	// 0.0.0.123, and 0x1f is hexadecimal.
	ipv4LikeName = regexp.MustCompile(`^(?:[0-9]+|0x[0-9a-f]*)$`)
)

// The server itself answers on these names; registering one would hide the
// dashboard.
var reservedNames = []string{"go", "hop", "localhost"}

func validateName(name string) error {
	switch {
	case !namePattern.MatchString(name):
		return fmt.Errorf("name %q can only use lowercase letters, digits and dashes", name)
	case ipv4LikeName.MatchString(name):
		return fmt.Errorf("name %q would be read as an IP address by browsers; add a letter", name)
	case slices.Contains(reservedNames, name):
		return fmt.Errorf("name %q is reserved for hop itself", name)
	}
	return nil
}

func validateService(s Service) error {
	if err := validateName(s.Name); err != nil {
		return err
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
	changed chan struct{} // signals the maintenance loop that names changed
	saveMu  sync.Mutex    // one save at a time, so an older snapshot never lands last

	mu     sync.Mutex
	byName map[string]Service
	dirty  bool
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
		// Names end up in /etc/hosts, so a hand-edited or tampered file must
		// not be able to slip in other host entries.
		if err := validateService(s); err != nil {
			slog.Warn("skipping invalid registry entry", "path", path, "err", err)
			continue
		}
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

// Put registers s. If the name is taken it replaces the old entry when
// replace is set, and otherwise returns a *NameTakenError, unless the old
// entry is the same registration: same target and same owning process.
// Visit history carries over, so an app restarting on a new port, or an
// undone removal, keeps its counts.
func (r *Registry) Put(s Service, replace bool) (Service, error) {
	if err := validateService(s); err != nil {
		return Service{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.byName[s.Name]; ok && !replace {
		// A matching target alone is not enough: a permanent shortcut must
		// not stay tied to an app's process, which would drop it on exit.
		if old.target() == s.target() && old.PID == s.PID {
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

// Remove deletes name and reports whether it did. With pid > 0 it deletes
// the name only while that process holds it, so an app that exits cannot
// remove a name a newer run of the app took over.
func (r *Registry) Remove(name string, pid int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.byName[name]
	if !ok || (pid > 0 && s.PID != pid) {
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

// SaveIfDirty writes the registry if it changed. A failed save is logged
// and retried on the next call.
func (r *Registry) SaveIfDirty() {
	r.saveMu.Lock()
	defer r.saveMu.Unlock()
	r.mu.Lock()
	dirty := r.dirty
	r.dirty = false
	r.mu.Unlock()
	if !dirty {
		return
	}
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
	return writeFileAtomic(r.path, b, 0o600)
}

// writeFileAtomic replaces path with b. A crash mid-write must never leave a
// half-written registry or hosts file, so it writes a temp file next to path,
// syncs it and renames it over the old one.
func writeFileAtomic(path string, b []byte, perm os.FileMode) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(b); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}
	// Without a sync, a power cut after the rename can leave an empty file.
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}
