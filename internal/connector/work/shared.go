package work

import (
	"slices"
	"sync"

	"github.com/kpenfound/hearsay/internal/connector"
)

// Shared is what the connectors of one process hold in common: the per-project
// locks a work source and its owner source take together, and each connector's
// gate, so that one can read and write the other's records. Neither can do so
// through its own sink, which the runtime confines to its own source.
//
// Locks are keyed by work source and project, so a work source's posts, its
// owner source's posts and both reconciliations of that project exclude each
// other. A deployment that ran the two sources in different processes would
// need a distributed lock instead; the shipped ones run one connectors process.
type Shared struct {
	mu     sync.Mutex
	locks  map[string]*sync.Mutex
	sinks  map[string]connector.Sink
	owners map[string][]string
}

// NewShared returns an empty Shared.
func NewShared() *Shared {
	return &Shared{locks: map[string]*sync.Mutex{}, sinks: map[string]connector.Sink{}, owners: map[string][]string{}}
}

func (s *Shared) lock(workSource, project string) func() {
	s.mu.Lock()
	key := workSource + "\x00" + project
	mu, ok := s.locks[key]
	if !ok {
		mu = &sync.Mutex{}
		s.locks[key] = mu
	}
	s.mu.Unlock()
	mu.Lock()
	return mu.Unlock
}

func (s *Shared) attach(source string, sink connector.Sink) {
	if sink == nil {
		return
	}
	s.mu.Lock()
	s.sinks[source] = sink
	s.mu.Unlock()
}

func (s *Shared) sink(source string) (connector.Sink, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sink, ok := s.sinks[source]
	return sink, ok
}

func (s *Shared) addOwner(workSource, owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !slices.Contains(s.owners[workSource], owner) {
		s.owners[workSource] = append(s.owners[workSource], owner)
	}
}

func (s *Shared) ownersOf(workSource string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.owners[workSource])
}
