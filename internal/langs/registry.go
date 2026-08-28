// file: internal/langs/registry.go
package langs

import (
	"fmt"
	"io/fs"
	"path"
	"sort"
	"sync"

	"gopkg.in/yaml.v3"
)

// Registry is an immutable-after-load set of language manifests.
//
// It loads from an fs.FS rather than a directory path so the same code serves both the
// embedded manifests baked into the binary (reproducible deploys) and a directory mounted
// at runtime (hot-adding a language without a rebuild).
type Registry struct {
	mu sync.RWMutex
	m  map[string]*Manifest
}

func NewRegistry() *Registry { return &Registry{m: map[string]*Manifest{}} }

// LoadFS reads every *.yaml under dir.
func (r *Registry) LoadFS(fsys fs.FS, dir string) error {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return fmt.Errorf("langs: read %s: %w", dir, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, e := range entries {
		if e.IsDir() || (path.Ext(e.Name()) != ".yaml" && path.Ext(e.Name()) != ".yml") {
			continue
		}
		b, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return fmt.Errorf("langs: read %s: %w", e.Name(), err)
		}
		var m Manifest
		m.Enabled = true // default on; a manifest opts out with `enabled: false`
		if err := yaml.Unmarshal(b, &m); err != nil {
			return fmt.Errorf("langs: parse %s: %w", e.Name(), err)
		}
		if err := m.Validate(); err != nil {
			return fmt.Errorf("langs: %s: %w", e.Name(), err)
		}
		if !m.Enabled {
			continue
		}
		if _, dup := r.m[m.ID]; dup {
			return fmt.Errorf("langs: duplicate id %q in %s", m.ID, e.Name())
		}
		r.m[m.ID] = &m
	}
	if len(r.m) == 0 {
		return fmt.Errorf("langs: no enabled manifests found in %s", dir)
	}
	return nil
}

func (r *Registry) Get(id string) (*Manifest, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.m[id]
	return m, ok
}

// List returns manifests sorted by id — stable output for the /v1/languages endpoint.
func (r *Registry) List() []*Manifest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Manifest, 0, len(r.m))
	for _, m := range r.m {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (r *Registry) IDs() []string {
	ms := r.List()
	ids := make([]string, len(ms))
	for i, m := range ms {
		ids[i] = m.ID
	}
	return ids
}
