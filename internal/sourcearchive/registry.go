package sourcearchive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
)

const (
	DefaultRegistryURL = "https://orkano-registry.orkano-system.svc.cluster.local"
	SourceMediaType    = "application/vnd.orkano.source.zip.v1"
	ArtifactMediaType  = "application/vnd.orkano.source.v1"
	manifestMediaType  = "application/vnd.oci.image.manifest.v1+json"
	configMediaType    = "application/vnd.oci.empty.v1+json"
)

var (
	appNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	digestPattern  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

type Registry struct {
	base   *url.URL
	client *http.Client
}

func NewRegistry(rawURL string, client *http.Client) (*Registry, error) {
	base, err := url.Parse(rawURL)
	if err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf("invalid source registry URL %q", rawURL)
	}
	base.Path = strings.TrimRight(base.Path, "/")
	if client == nil {
		client = &http.Client{}
	}
	registryClient := *client
	previousCheckRedirect := registryClient.CheckRedirect
	registryClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != base.Scheme || req.URL.Host != base.Host || req.URL.User != nil {
			return errors.New("source registry refused a cross-origin redirect")
		}
		if previousCheckRedirect != nil {
			return previousCheckRedirect(req, via)
		}
		if len(via) >= 10 {
			return errors.New("source registry stopped after 10 redirects")
		}
		return nil
	}
	return &Registry{base: base, client: &registryClient}, nil
}

func NewTLSRegistry(rawURL, caFile string) (*Registry, error) {
	// caFile is an installation-owned command-line setting, not archive input.
	pem, err := os.ReadFile(caFile) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("read source registry CA: %w", err)
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("load system certificate pool: %w", err)
	}
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("source registry CA contains no certificates")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	return NewRegistry(rawURL, &http.Client{Transport: transport})
}

func (r *Registry) Upload(ctx context.Context, appName, filename string, source io.ReadSeeker, size int64, digest string) error {
	if err := validateReference(appName, digest); err != nil {
		return err
	}
	if size < 1 || size > MaxCompressedBytes {
		return fmt.Errorf("source archive size %d is outside the 1..%d byte limit", size, MaxCompressedBytes)
	}
	if err := r.uploadBlob(ctx, appName, source, size, digest, SourceMediaType); err != nil {
		return err
	}

	config := []byte("{}")
	configSum := sha256.Sum256(config)
	configDigest := "sha256:" + hex.EncodeToString(configSum[:])
	if err := r.uploadBlob(ctx, appName, bytes.NewReader(config), int64(len(config)), configDigest, configMediaType); err != nil {
		return err
	}

	manifest := struct {
		SchemaVersion int               `json:"schemaVersion"`
		MediaType     string            `json:"mediaType"`
		ArtifactType  string            `json:"artifactType"`
		Config        descriptor        `json:"config"`
		Layers        []descriptor      `json:"layers"`
		Annotations   map[string]string `json:"annotations,omitempty"`
	}{
		SchemaVersion: 2,
		MediaType:     manifestMediaType,
		ArtifactType:  ArtifactMediaType,
		Config:        descriptor{MediaType: configMediaType, Digest: configDigest, Size: int64(len(config))},
		Layers:        []descriptor{{MediaType: SourceMediaType, Digest: digest, Size: size}},
		Annotations:   map[string]string{"org.opencontainers.image.title": filename},
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("encode source manifest: %w", err)
	}
	tag := strings.TrimPrefix(digest, "sha256:")
	requestURL := r.endpoint("v2", sourceRepository(appName), "manifests", tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, requestURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create source manifest request: %w", err)
	}
	req.Header.Set("Content-Type", manifestMediaType)
	req.ContentLength = int64(len(body))
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("upload source manifest: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted {
		return registryStatusError("upload source manifest", resp)
	}
	return nil
}

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

func (r *Registry) uploadBlob(ctx context.Context, appName string, source io.ReadSeeker, size int64, digest, mediaType string) error {
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind source blob: %w", err)
	}
	startURL := r.endpoint("v2", sourceRepository(appName), "blobs", "uploads") + "/"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, startURL, nil)
	if err != nil {
		return fmt.Errorf("create source upload request: %w", err)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("start source blob upload: %w", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		defer func() { _ = resp.Body.Close() }()
		return registryStatusError("start source blob upload", resp)
	}
	location := resp.Header.Get("Location")
	if err := resp.Body.Close(); err != nil {
		return fmt.Errorf("close source blob upload response: %w", err)
	}
	uploadURL, err := r.resolveUploadLocation(location)
	if err != nil {
		return err
	}
	query := uploadURL.Query()
	query.Set("digest", digest)
	uploadURL.RawQuery = query.Encode()
	req, err = http.NewRequestWithContext(ctx, http.MethodPut, uploadURL.String(), source)
	if err != nil {
		return fmt.Errorf("create source blob PUT: %w", err)
	}
	req.Header.Set("Content-Type", mediaType)
	req.ContentLength = size
	resp, err = r.client.Do(req)
	if err != nil {
		return fmt.Errorf("upload source blob: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted {
		return registryStatusError("upload source blob", resp)
	}
	return nil
}

func (r *Registry) Download(ctx context.Context, appName, digest string, destination io.Writer) error {
	if err := validateReference(appName, digest); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.endpoint("v2", sourceRepository(appName), "blobs", digest), nil)
	if err != nil {
		return fmt.Errorf("create source download request: %w", err)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("download source archive: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return registryStatusError("download source archive", resp)
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(destination, hash), io.LimitReader(resp.Body, MaxCompressedBytes+1))
	if err != nil {
		return fmt.Errorf("read source archive: %w", err)
	}
	if written > MaxCompressedBytes {
		return fmt.Errorf("source archive exceeds the %d-byte limit", MaxCompressedBytes)
	}
	actual := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if actual != digest {
		return fmt.Errorf("source archive digest mismatch: got %s, want %s", actual, digest)
	}
	return nil
}

func (r *Registry) endpoint(parts ...string) string {
	u := *r.base
	joined := append([]string{u.Path}, parts...)
	u.Path = path.Join(joined...)
	return u.String()
}

func (r *Registry) resolveUploadLocation(location string) (*url.URL, error) {
	if location == "" {
		return nil, errors.New("source registry returned no upload location")
	}
	parsed, err := url.Parse(location)
	if err != nil {
		return nil, fmt.Errorf("parse source registry upload location: %w", err)
	}
	resolved := r.base.ResolveReference(parsed)
	if resolved.Scheme != r.base.Scheme || resolved.Host != r.base.Host || resolved.User != nil {
		return nil, errors.New("source registry returned a cross-origin upload location")
	}
	return resolved, nil
}

func validateReference(appName, digest string) error {
	if !appNamePattern.MatchString(appName) || len(appName) > 253 {
		return fmt.Errorf("invalid source archive app name %q", appName)
	}
	if !digestPattern.MatchString(digest) {
		return fmt.Errorf("invalid source archive digest %q", digest)
	}
	return nil
}

func sourceRepository(appName string) string {
	sum := sha256.Sum256([]byte(appName))
	return "orkano-sources/app-" + hex.EncodeToString(sum[:])
}

func registryStatusError(action string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	detail := strings.TrimSpace(string(body))
	if detail == "" {
		detail = http.StatusText(resp.StatusCode)
	}
	return fmt.Errorf("%s: registry returned %d: %s", action, resp.StatusCode, strconv.Quote(detail))
}
