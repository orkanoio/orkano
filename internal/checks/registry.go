// Package checks runs a registry of capability checks (the api/check contract)
// in dependency order and reports the results to the three consumers named in
// that package: the install preflight, the onboarding wizard, and orkano
// doctor. It is the engine — the checks themselves are composed by each
// consumer and registered here; this package never reads configuration, it
// only walks the graph the checks declare and executes their probes.
package checks

import (
	"cmp"
	"errors"
	"fmt"
	"slices"

	"github.com/orkanoio/orkano/api/check"
)

// Registration errors. A malformed check is a programming error, so Register
// reports it and MustRegister turns it into a panic at startup — a contributed
// check that cannot be registered must fail loudly, not silently vanish.
var (
	ErrEmptyID         = errors.New("check ID is empty")
	ErrNoProbe         = errors.New("check has no Probe")
	ErrInvalidSeverity = errors.New("check severity is not one of critical/warning/info")
	ErrDuplicateID     = errors.New("check ID already registered")
)

// Graph errors surface when the dependency graph cannot be walked. They are
// returned by Plan and Run and never swallowed: an unrunnable graph must not
// be mistaken for a hardened install.
var (
	ErrMissingRequirement = errors.New("check requires an unregistered check")
	ErrCycle              = errors.New("check requirement cycle")
)

// Registry holds the checks a single consumer will run, keyed by ID. It is not
// safe for concurrent registration; checks are registered once at startup and
// the registry is then read-only.
type Registry struct {
	byID  map[string]check.Check
	order []string // registration order — the stable tie-break for the walk
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{byID: make(map[string]check.Check)}
}

// Register validates and stores a check. It rejects an empty ID, a missing
// Probe, an unknown severity, and a duplicate ID; it does not validate Requires
// (those IDs may not be registered yet) — Plan does that once the graph is
// complete.
func (r *Registry) Register(c check.Check) error {
	if c.ID == "" {
		// Returned bare on purpose: c.ID is "" here, so there is nothing to
		// qualify the sentinel with.
		return ErrEmptyID
	}
	if c.Probe == nil {
		return fmt.Errorf("%q: %w", c.ID, ErrNoProbe)
	}
	switch c.Severity {
	case check.SeverityCritical, check.SeverityWarning, check.SeverityInfo:
	default:
		return fmt.Errorf("%q: %w", c.ID, ErrInvalidSeverity)
	}
	if _, dup := r.byID[c.ID]; dup {
		return fmt.Errorf("%q: %w", c.ID, ErrDuplicateID)
	}

	// Store a copy with Requires de-duplicated: a requirement listed twice is a
	// contributor slip, and a raw duplicate would otherwise be counted twice in
	// a dependent's Blockers and rendered twice in the output.
	c.Requires = dedupe(c.Requires)
	r.byID[c.ID] = c
	r.order = append(r.order, c.ID)
	return nil
}

// dedupe removes duplicate IDs, preserving first-occurrence order. The result
// never aliases the input's backing array.
func dedupe(ids []string) []string {
	if len(ids) < 2 {
		return ids
	}
	seen := make(map[string]struct{}, len(ids))
	out := ids[:0:0] // zero-cap slice over a fresh array; append never aliases the caller
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// MustRegister registers a check and panics if it cannot. It is the form used
// for static registration where a bad check is a build-time bug.
func (r *Registry) MustRegister(c check.Check) {
	if err := r.Register(c); err != nil {
		panic("checks: " + err.Error())
	}
}

// Get returns the registered check with the given ID.
func (r *Registry) Get(id string) (check.Check, bool) {
	c, ok := r.byID[id]
	return c, ok
}

// Len reports how many checks are registered.
func (r *Registry) Len() int { return len(r.order) }

// Plan validates the dependency graph and returns the checks in execution
// order: every check appears after all the checks in its Requires. Among checks
// that become ready at the same time, registration order is preserved, so the
// wizard walks them predictably and JSON output is stable.
//
// Plan returns ErrMissingRequirement if a Requires names an unregistered check
// and ErrCycle if the requirements form a loop (including a check that requires
// itself); both wrap the offending IDs.
func (r *Registry) Plan() ([]check.Check, error) {
	// registration index, used to break ties deterministically.
	idx := make(map[string]int, len(r.order))
	for i, id := range r.order {
		idx[id] = i
	}

	indegree := make(map[string]int, len(r.order))
	dependents := make(map[string][]string, len(r.order))
	for _, id := range r.order {
		for _, req := range r.byID[id].Requires {
			if _, ok := r.byID[req]; !ok {
				return nil, fmt.Errorf("%q requires %q: %w", id, req, ErrMissingRequirement)
			}
			indegree[id]++
			dependents[req] = append(dependents[req], id)
		}
	}

	// Kahn's algorithm: repeatedly emit a check with no unmet requirement,
	// preferring the earliest-registered among those currently ready.
	var ready []string
	for _, id := range r.order {
		if indegree[id] == 0 {
			ready = append(ready, id)
		}
	}

	order := make([]check.Check, 0, len(r.order))
	for len(ready) > 0 {
		slices.SortFunc(ready, func(a, b string) int { return cmp.Compare(idx[a], idx[b]) })
		id := ready[0]
		ready = ready[1:]
		order = append(order, r.byID[id])
		for _, dep := range dependents[id] {
			indegree[dep]--
			if indegree[dep] == 0 {
				ready = append(ready, dep)
			}
		}
	}

	if len(order) < len(r.order) {
		// Whatever still has unmet requirements is in, or downstream of, a cycle.
		var stuck []string
		for _, id := range r.order {
			if indegree[id] > 0 {
				stuck = append(stuck, id)
			}
		}
		return nil, fmt.Errorf("involving %v: %w", stuck, ErrCycle)
	}
	return order, nil
}
