package agents

import (
	"fmt"
	"sort"
	"sync"
)

type Manager struct {
	mu       sync.RWMutex
	profiles map[string]Profile
}

func NewManager(profiles []Profile) (*Manager, error) {
	m := &Manager{profiles: make(map[string]Profile)}
	for _, p := range profiles {
		if err := p.Validate(); err != nil {
			return nil, fmt.Errorf("invalid profile %q: %w", p.ID, err)
		}
		m.profiles[p.ID] = p
	}
	return m, nil
}

func NewBuiltinManager() (*Manager, error) {
	return NewManager(BuiltinProfiles())
}

func (m *Manager) Get(id string) (Profile, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.profiles[id]
	return p, ok
}

func (m *Manager) List() []Profile {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Profile, 0, len(m.profiles))
	for _, p := range m.profiles {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
