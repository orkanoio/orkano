// Package repoallowlist owns the shared GitHub repository allowlist format.
package repoallowlist

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	Namespace       = "orkano-system"
	ConfigMapName   = "orkano-repo-allowlist"
	DataKey         = "repositories"
	DefaultFilePath = "/etc/orkano/repo-allowlist/repositories"
	SeedSubcommand  = "bootstrap-repo-allowlist"

	MaxRepositoryLength = 140
	RepositoryPattern   = `^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`

	// Four hundred maximum-length entries stay below both Linux's 128 KiB
	// per-string exec limit (Helm's seed env) and the installer's 96 KiB inline
	// base64 command limit.
	MaxRepositories = 400
)

var repositoryPattern = regexp.MustCompile(RepositoryPattern)

// Normalize validates repositories and returns their canonical policy form:
// lowercase, de-duplicated, and sorted.
func Normalize(repositories []string) ([]string, error) {
	normalized := make(map[string]struct{}, len(repositories))
	nonEmpty := 0
	for _, repository := range repositories {
		repository = strings.TrimSpace(repository)
		if repository == "" {
			continue
		}
		nonEmpty++
		if nonEmpty > MaxRepositories {
			return nil, fmt.Errorf("repository allowlist has more than %d entries", MaxRepositories)
		}
		if len(repository) > MaxRepositoryLength {
			return nil, fmt.Errorf("repository %q is longer than %d characters", repository, MaxRepositoryLength)
		}
		if !repositoryPattern.MatchString(repository) {
			return nil, fmt.Errorf("invalid repository %q (want owner/repository)", repository)
		}
		normalized[strings.ToLower(repository)] = struct{}{}
	}
	out := make([]string, 0, len(normalized))
	for repository := range normalized {
		out = append(out, repository)
	}
	sort.Strings(out)
	return out, nil
}

// Parse reads the newline-delimited ConfigMap value.
func Parse(raw string) ([]string, error) {
	return Normalize(strings.Split(raw, "\n"))
}

// Format returns the canonical newline-delimited ConfigMap value.
func Format(repositories []string) (string, error) {
	normalized, err := Normalize(repositories)
	if err != nil {
		return "", err
	}
	if len(normalized) == 0 {
		return "", nil
	}
	return strings.Join(normalized, "\n") + "\n", nil
}
