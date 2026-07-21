// Package features defines Orkano's explicit, default-off unsafe feature
// gates. It is the single source of truth shared by installers, API surfaces,
// the dispatcher, and reconcilers.
package features

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

// ID is a stable feature identifier used in CLI flags, Helm values, process
// configuration, API errors, and doctor output. Existing values must never be
// renamed or reused for a different capability.
type ID string

const (
	SourceGit     ID = "source.git"
	SourceZip     ID = "source.zip"
	BuildNixpacks ID = "build.nixpacks"
)

// Definition is immutable product metadata for one feature gate.
type Definition struct {
	ID          ID
	Name        string
	Description string
	Unsafe      bool
}

var definitions = []Definition{
	{
		ID:          SourceGit,
		Name:        "Generic Git",
		Description: "Build from an unauthenticated public HTTPS Git repository without GitHub App provenance or automatic webhooks.",
		Unsafe:      true,
	},
	{
		ID:          SourceZip,
		Name:        "ZIP upload",
		Description: "Build an uploaded archive with no Git commit provenance. In v1, any build job can read uploaded source archives from the shared registry.",
		Unsafe:      true,
	},
	{
		ID:          BuildNixpacks,
		Name:        "Nixpacks",
		Description: "Generate a Dockerfile with the maintenance-mode Nixpacks tool before the isolated BuildKit build.",
		Unsafe:      true,
	},
}

var known = func() map[ID]struct{} {
	values := make(map[ID]struct{}, len(definitions))
	for _, definition := range definitions {
		values[definition.ID] = struct{}{}
	}
	return values
}()

// Definitions returns a copy in stable product-display order.
func Definitions() []Definition {
	return append([]Definition(nil), definitions...)
}

// Set is the set of explicitly enabled features. Its zero value enables
// nothing, preserving secure-by-default behavior when configuration is absent.
type Set struct {
	enabled map[ID]struct{}
}

// Parse validates and de-duplicates explicit feature IDs. Whitespace around
// each value is ignored, but an empty or unknown value is rejected so a typo
// can never silently weaken or change installation behavior.
func Parse(values []string) (Set, error) {
	set := Set{enabled: make(map[ID]struct{}, len(values))}
	for _, raw := range values {
		id := ID(strings.TrimSpace(raw))
		if id == "" {
			return Set{}, fmt.Errorf("features: feature ID must not be empty")
		}
		if _, ok := known[id]; !ok {
			return Set{}, fmt.Errorf("features: unknown unsafe feature %q (allowed: %s)", id, allowedCSV())
		}
		set.enabled[id] = struct{}{}
	}
	return set, nil
}

// ParseCSV parses the canonical process-environment representation. An empty
// string is the secure default: no unsafe features are enabled.
func ParseCSV(value string) (Set, error) {
	if strings.TrimSpace(value) == "" {
		return Parse(nil)
	}
	return Parse(strings.Split(value, ","))
}

// Enabled reports whether id was explicitly enabled. Unknown IDs are always
// disabled; startup configuration should be validated through Parse first.
func (s Set) Enabled(id ID) bool {
	_, ok := s.enabled[id]
	return ok
}

// IDs returns enabled IDs in deterministic lexical order.
func (s Set) IDs() []ID {
	ids := make([]ID, 0, len(s.enabled))
	for id := range s.enabled {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// CSV returns the canonical, deterministic process-environment
// representation of the set.
func (s Set) CSV() string {
	ids := s.IDs()
	values := make([]string, len(ids))
	for i := range ids {
		values[i] = string(ids[i])
	}
	return strings.Join(values, ",")
}

// RequiredForApp returns the unsafe gates used by spec in stable product
// order. Core GitHub, Dockerfile, and Static configurations require no gate.
func RequiredForApp(spec orkanov1alpha1.AppSpec) []ID {
	required := make([]ID, 0, 2)
	if spec.Source.Git != nil {
		required = append(required, SourceGit)
	}
	if spec.Source.Upload != nil {
		required = append(required, SourceZip)
	}
	if spec.Build.Strategy == orkanov1alpha1.StrategyNixpacks {
		required = append(required, BuildNixpacks)
	}
	return required
}

// MissingForApp returns the gates required by spec but not enabled in s.
func (s Set) MissingForApp(spec orkanov1alpha1.AppSpec) []ID {
	required := RequiredForApp(spec)
	missing := make([]ID, 0, len(required))
	for _, id := range required {
		if !s.Enabled(id) {
			missing = append(missing, id)
		}
	}
	return missing
}

// ErrDisabled identifies app specifications that use default-off features.
var ErrDisabled = errors.New("required unsafe feature is disabled")

// DisabledError carries every disabled gate used by an app specification.
type DisabledError struct {
	IDs []ID
}

func (e *DisabledError) Error() string {
	values := make([]string, len(e.IDs))
	for i := range e.IDs {
		values[i] = string(e.IDs[i])
	}
	return fmt.Sprintf("features: %s: %s", ErrDisabled, strings.Join(values, ","))
}

func (e *DisabledError) Unwrap() error {
	return ErrDisabled
}

// ValidateApp rejects a specification that uses any gate not explicitly
// enabled in s.
func (s Set) ValidateApp(spec orkanov1alpha1.AppSpec) error {
	missing := s.MissingForApp(spec)
	if len(missing) == 0 {
		return nil
	}
	return &DisabledError{IDs: missing}
}

func allowedCSV() string {
	ids := make([]string, len(definitions))
	for i := range definitions {
		ids[i] = string(definitions[i].ID)
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}
