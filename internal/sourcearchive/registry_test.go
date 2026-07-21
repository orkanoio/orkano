package sourcearchive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegistryUploadAndDownload(t *testing.T) {
	source := []byte("PK source archive")
	appName := strings.Repeat("a", 249)
	sum := sha256.Sum256(source)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	var blobs = map[string][]byte{}
	var manifest []byte
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/v2/"+sourceRepository(appName)+"/") {
			http.Error(w, "unexpected source repository", http.StatusBadRequest)
			return
		}
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/blobs/uploads/"):
			w.Header().Set("Location", r.URL.Path+"session")
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/blobs/uploads/session"):
			body, _ := io.ReadAll(r.Body)
			blobs[r.URL.Query().Get("digest")] = body
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/manifests/"):
			manifest, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/blobs/"):
			body, ok := blobs[r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(body)
		default:
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	registry, err := NewRegistry(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Upload(context.Background(), appName, "source.zip", bytes.NewReader(source), int64(len(source)), digest); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if !bytes.Equal(blobs[digest], source) {
		t.Fatalf("source blob = %q", blobs[digest])
	}
	var decoded struct {
		ArtifactType string            `json:"artifactType"`
		Layers       []descriptor      `json:"layers"`
		Annotations  map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(manifest, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ArtifactType != ArtifactMediaType || len(decoded.Layers) != 1 || decoded.Layers[0].Digest != digest || decoded.Annotations["org.opencontainers.image.title"] != "source.zip" {
		t.Fatalf("manifest = %+v", decoded)
	}
	var downloaded bytes.Buffer
	if err := registry.Download(context.Background(), appName, digest, &downloaded); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if !bytes.Equal(downloaded.Bytes(), source) {
		t.Fatalf("download = %q", downloaded.Bytes())
	}
}

func TestSourceRepositoryIsBoundedAndAppScoped(t *testing.T) {
	long := strings.Repeat("a", 253)
	repository := sourceRepository(long)
	if len(repository) > 255 || strings.Contains(repository, long) {
		t.Fatalf("repository = %q (len %d)", repository, len(repository))
	}
	if repository == sourceRepository(long[:252]+"b") {
		t.Fatal("different Apps share a source repository")
	}
}

func TestRegistryRefusesCrossOriginUpload(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://attacker.example/upload")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	registry, err := NewRegistry(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("x")
	sum := sha256.Sum256(body)
	err = registry.Upload(context.Background(), "demo", "source.zip", bytes.NewReader(body), 1, "sha256:"+hex.EncodeToString(sum[:]))
	if err == nil || !strings.Contains(err.Error(), "cross-origin") {
		t.Fatalf("Upload error = %v", err)
	}
}

func TestRegistryRefusesCrossOriginRedirect(t *testing.T) {
	targetHit := false
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetHit = true
	}))
	defer target.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL+"/stolen")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	registry, err := NewRegistry(origin.URL, origin.Client())
	if err != nil {
		t.Fatal(err)
	}
	err = registry.Download(context.Background(), "demo", "sha256:"+strings.Repeat("0", 64), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "cross-origin redirect") {
		t.Fatalf("Download error = %v", err)
	}
	if targetHit {
		t.Fatal("cross-origin redirect reached target")
	}
}

func TestRegistryDownloadVerifiesDigest(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("tampered"))
	}))
	defer server.Close()
	registry, err := NewRegistry(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("0", 64)
	err = registry.Download(context.Background(), "demo", digest, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("Download error = %v", err)
	}
}
