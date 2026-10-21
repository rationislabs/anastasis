// Package state persists the projects anastasis manages in one JSON file.
// What is running is never stored here; it is always read from the runtime.
package state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"

	"github.com/rationislabs/anastasis/internal/model"
)

// ProjectState is everything anastasis remembers about one project.
type ProjectState struct {
	Source   string            `json:"source"` // absolute path of the Compose file
	Current  model.Project     `json:"current"`
	Previous *model.Project    `json:"previous,omitempty"` // for failure_action: rollback
	Scale    map[string]int    `json:"scale,omitempty"`    // anastasis scale overrides
	Salt     map[string]string `json:"salt,omitempty"`     // set by rollout restart
}

// Store is the state file plus an exclusive lock on it.
type Store struct {
	path     string
	lock     *os.File
	mu       sync.Mutex
	projects map[string]*ProjectState
}

// Open loads path, creating its directory if needed, and locks it so only
// one anastasis serve uses it.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("%s is locked; is another anastasis serve running? (%w)", path, err)
	}
	s := &Store{path: path, lock: lock, projects: map[string]*ProjectState{}}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		lock.Close()
		return nil, err
	default:
		if err := json.Unmarshal(b, &s.projects); err != nil {
			lock.Close()
			return nil, fmt.Errorf("state file %s is corrupt and was left untouched: %w", path, err)
		}
	}
	return s, nil
}

// Close releases the lock.
func (s *Store) Close() error { return s.lock.Close() }

// List returns copies of all projects, sorted by name.
func (s *Store) List() []ProjectState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ProjectState, 0, len(s.projects))
	for _, p := range s.projects {
		out = append(out, p.clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Current.Name < out[j].Current.Name })
	return out
}

// Get returns a copy of one project.
func (s *Store) Get(name string) (ProjectState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projects[name]
	if !ok {
		return ProjectState{}, false
	}
	return p.clone(), true
}

// Up registers p, loaded from source, or replaces its config. The old
// config becomes Previous, scale overrides reset and rollout salts are
// kept. A project name registered from another directory is refused, so two
// stacks in directories with the same name never replace each other; another
// file in the same directory (compose.yaml to compose.json) is the same
// project.
func (s *Store) Up(p model.Project, source string) error {
	return s.update(func(m map[string]*ProjectState) error {
		ps := &ProjectState{Source: source, Current: p}
		if old, ok := m[p.Name]; ok {
			if filepath.Dir(old.Source) != filepath.Dir(source) {
				return fmt.Errorf("project %q is already registered from %s; give one of them a different top-level name:", p.Name, old.Source)
			}
			prev := old.Current
			ps.Previous = &prev
			ps.Salt = old.Salt
		}
		m[p.Name] = ps
		return nil
	})
}

// Down unregisters a project.
func (s *Store) Down(name string) error {
	return s.update(func(m map[string]*ProjectState) error {
		if _, ok := m[name]; !ok {
			return fmt.Errorf("project %q is not registered", name)
		}
		delete(m, name)
		return nil
	})
}

// Scale sets replica overrides that last until the project's next Up.
func (s *Store) Scale(project string, scale map[string]int) error {
	return s.update(func(m map[string]*ProjectState) error {
		ps, ok := m[project]
		if !ok {
			return fmt.Errorf("project %q is not registered", project)
		}
		for name, n := range scale {
			svc, ok := ps.Current.Services[name]
			if !ok {
				return fmt.Errorf("project %q has no service %q", project, name)
			}
			if svc.Autoscale != nil {
				return fmt.Errorf("service %q is autoscaled; change min/max in the Compose file", name)
			}
			if n < 0 {
				return fmt.Errorf("replicas for %q must be >= 0", name)
			}
			if n > 1 && len(svc.Config.Ports) > 0 {
				return fmt.Errorf("service %q uses ports: and cannot run more than one replica; use x-anastasis.expose", name)
			}
			if ps.Scale == nil {
				ps.Scale = map[string]int{}
			}
			ps.Scale[name] = n
		}
		return nil
	})
}

// Restart gives a service a new rollout salt, which changes its hash and
// so rolls every replica.
func (s *Store) Restart(project, service string) error {
	return s.update(func(m map[string]*ProjectState) error {
		ps, ok := m[project]
		if !ok {
			return fmt.Errorf("project %q is not registered", project)
		}
		if _, ok := ps.Current.Services[service]; !ok {
			return fmt.Errorf("project %q has no service %q", project, service)
		}
		b := make([]byte, 4)
		rand.Read(b)
		if ps.Salt == nil {
			ps.Salt = map[string]string{}
		}
		ps.Salt[service] = hex.EncodeToString(b)
		return nil
	})
}

// Rollback swaps a service's current and previous config. It reports false
// when there is no different previous config to go back to.
func (s *Store) Rollback(project, service string) (bool, error) {
	swapped := false
	err := s.update(func(m map[string]*ProjectState) error {
		ps, ok := m[project]
		if !ok || ps.Previous == nil {
			return nil
		}
		prev, ok := ps.Previous.Services[service]
		cur := ps.Current.Services[service]
		if !ok || prev.Hash == cur.Hash {
			return nil
		}
		// ponytail: a rollback whose target also fails rolls back again; add a rollback limit if that ping-pong shows up.
		ps.Current.Services[service], ps.Previous.Services[service] = prev, cur
		swapped = true
		return nil
	})
	return swapped, err
}

// update applies f to a copy of the projects and keeps the copy only if it
// was saved, so memory and disk never disagree.
func (s *Store) update(f func(map[string]*ProjectState) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]*ProjectState, len(s.projects))
	for k, v := range s.projects {
		c := v.clone()
		next[k] = &c
	}
	if err := f(next); err != nil {
		return err
	}
	return s.save(next)
}

// save writes projects and, once saved, keeps them. Caller holds mu.
func (s *Store) save(projects map[string]*ProjectState) error {
	if err := write(s.path, projects); err != nil {
		return err
	}
	s.projects = projects
	return nil
}

// write writes to a temp file and renames it, so a crash never leaves a
// half-written state file.
func write(path string, projects map[string]*ProjectState) error {
	b, err := json.MarshalIndent(projects, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*.json") // mode 0600
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (p ProjectState) clone() ProjectState {
	c := ProjectState{Source: p.Source, Current: cloneProject(p.Current), Scale: maps.Clone(p.Scale), Salt: maps.Clone(p.Salt)}
	if p.Previous != nil {
		prev := cloneProject(*p.Previous)
		c.Previous = &prev
	}
	return c
}

func cloneProject(p model.Project) model.Project {
	p.Services = maps.Clone(p.Services)
	return p
}
