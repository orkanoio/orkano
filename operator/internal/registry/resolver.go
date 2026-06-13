// Package registry resolves a pushed tag to its digest-pinned reference by
// asking the registry itself — a manifest HEAD, never log parsing (INV-06).
// TLS is verified against the cluster-internal CA bundle the install
// publishes for build pods (ConfigMap orkano-registry-ca in orkano-builds);
// the operator reads the same projection, so there is exactly one trust
// root to rotate.
package registry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/orkanoio/orkano/operator/internal/buildjob"
)

const (
	caKey          = "ca.crt"
	requestTimeout = 10 * time.Second
)

// acceptedManifestTypes covers what BuildKit may have pushed under the tag:
// Docker schema 2 and OCI, single manifest or index. Whichever the registry
// returns, its Docker-Content-Digest is the digest kubelet pulls by.
const acceptedManifestTypes = "application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.oci.image.index.v1+json, " +
	"application/vnd.docker.distribution.manifest.v2+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json"

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Resolver turns "host/repo:tag" into "host/repo@sha256:…" with one HEAD
// request. Reader must be uncached (manager.GetAPIReader): the CA ConfigMap
// is fetched per resolution, which keeps the operator's RBAC at a single
// resourceNames-pinned get — no informer, no list/watch grant.
type Resolver struct {
	Reader client.Reader
}

func (r *Resolver) ResolveDigest(ctx context.Context, imageRef string) (string, error) {
	host, repo, tag, err := parseRef(imageRef)
	if err != nil {
		return "", err
	}
	pool, err := r.caPool(ctx)
	if err != nil {
		return "", err
	}

	httpClient := &http.Client{
		Timeout: requestTimeout,
		Transport: &http.Transport{
			// The transport lives for one request (the CA pool is re-read
			// per call so rotation takes effect); without this, each call
			// would strand an idle connection and its goroutine.
			DisableKeepAlives: true,
			TLSClientConfig:   &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
	}
	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", host, repo, tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return "", fmt.Errorf("building manifest HEAD request: %w", err)
	}
	req.Header.Set("Accept", acceptedManifestTypes)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("manifest HEAD %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("manifest HEAD %s: registry answered %s", url, resp.Status)
	}
	digest := resp.Header.Get("Docker-Content-Digest")
	if !digestPattern.MatchString(digest) {
		return "", fmt.Errorf("manifest HEAD %s: registry answered without a usable Docker-Content-Digest header (%q)", url, digest)
	}
	return host + "/" + repo + "@" + digest, nil
}

func (r *Resolver) caPool(ctx context.Context) (*x509.CertPool, error) {
	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: buildjob.Namespace, Name: buildjob.CAConfigMapName}
	if err := r.Reader.Get(ctx, key, &cm); err != nil {
		return nil, fmt.Errorf("fetching registry CA bundle %s/%s: %w", key.Namespace, key.Name, err)
	}
	pem, ok := cm.Data[caKey]
	if !ok || pem == "" {
		return nil, fmt.Errorf("registry CA bundle %s/%s has no %s key", key.Namespace, key.Name, caKey)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(pem)) {
		return nil, fmt.Errorf("registry CA bundle %s/%s: %s holds no parseable certificate", key.Namespace, key.Name, caKey)
	}
	return pool, nil
}

// parseRef splits "host/repo[/sub…]:tag". The host is whatever precedes the
// first slash — every ref the Build controller composes starts with the
// canonical registry host (or a test server's host:port), so no docker.io
// shorthand handling exists on purpose.
func parseRef(imageRef string) (host, repo, tag string, err error) {
	host, rest, found := strings.Cut(imageRef, "/")
	if !found || host == "" || rest == "" {
		return "", "", "", fmt.Errorf("image ref %q has no host/repository split", imageRef)
	}
	if strings.Contains(rest, "@") {
		return "", "", "", fmt.Errorf("image ref %q is already digest-pinned", imageRef)
	}
	i := strings.LastIndex(rest, ":")
	if i <= 0 || i == len(rest)-1 {
		return "", "", "", fmt.Errorf("image ref %q has no tag to resolve", imageRef)
	}
	repo, tag = rest[:i], rest[i+1:]
	if strings.Contains(tag, "/") {
		return "", "", "", fmt.Errorf("image ref %q has no tag to resolve", imageRef)
	}
	return host, repo, tag, nil
}
