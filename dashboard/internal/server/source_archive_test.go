package server

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
	"github.com/orkanoio/orkano/dashboard/internal/auth"
	"github.com/orkanoio/orkano/internal/features"
)

type fakeArchiveStore struct {
	appName  string
	filename string
	digest   string
	body     []byte
	err      error
}

func (f *fakeArchiveStore) Upload(_ context.Context, appName, filename string, source io.ReadSeeker, _ int64, digest string) error {
	f.appName, f.filename, f.digest = appName, filename, digest
	f.body, _ = io.ReadAll(source)
	return f.err
}

func TestFeaturesEndpointUsesSecureDefaults(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := authedSession(t, store)
	rec := apiReq(t, s, http.MethodGet, "/api/features", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("features = %d (%s)", rec.Code, rec.Body.String())
	}
	featuresBody, ok := decodeBody(t, rec)["features"].([]any)
	if !ok || len(featuresBody) != 3 {
		t.Fatalf("features = %#v", featuresBody)
	}
	for _, raw := range featuresBody {
		item := raw.(map[string]any)
		if item["unsafe"] != true || item["enabled"] != false {
			t.Fatalf("feature = %#v, want unsafe and disabled", item)
		}
	}
}

func TestCreateAppRejectsDisabledUnsafeSource(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := authedSession(t, store)
	spec := webAppSpec()
	spec.Source.GitHub = nil
	spec.Source.Git = &gitSourceForTest
	rec := apiReq(t, s, http.MethodPost, "/api/apps", appCreateRequest{Name: "generic", Spec: spec}, ck)
	if rec.Code != http.StatusForbidden || decodeBody(t, rec)["error"] != "feature_disabled" {
		t.Fatalf("create = %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestDisabledUnsafeSourceAllowsRuntimeUpdateButBlocksNewBuildInput(t *testing.T) {
	store := newFakeStore()
	app := seedApp("generic")
	app.Spec.Source.GitHub = nil
	app.Spec.Source.Git = &gitSourceForTest
	s := configuredAPIServer(t, store, features.Set{}, nil, app)
	ck := authedSession(t, store)

	runtimeOnly := app.Spec.DeepCopy()
	replicas := int32(2)
	runtimeOnly.Replicas = &replicas
	rec := apiReq(t, s, http.MethodPut, "/api/apps/generic", appUpdateRequest{Spec: *runtimeOnly}, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("runtime update = %d (%s)", rec.Code, rec.Body.String())
	}

	changedSource := runtimeOnly.Source.DeepCopy()
	changedSource.Git.Ref = "release"
	rec = apiReq(t, s, http.MethodPut, "/api/apps/generic/source", appSourceUpdateRequest{Source: *changedSource, Build: runtimeOnly.Build}, ck)
	if rec.Code != http.StatusForbidden || decodeBody(t, rec)["error"] != "feature_disabled" {
		t.Fatalf("source update = %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestUploadSourceArchive(t *testing.T) {
	archive := validSourceZIP(t)
	store := newFakeStore()
	featureSet, err := features.Parse([]string{string(features.SourceZip)})
	if err != nil {
		t.Fatal(err)
	}
	archiveStore := &fakeArchiveStore{}
	s := configuredAPIServer(t, store, featureSet, archiveStore, seedApp("demo"))
	ck := authedSession(t, store)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/apps/demo/source/archive", bytes.NewReader(archive))
	req.Header.Set("Content-Type", "application/zip")
	req.Header.Set("X-Orkano-Filename", "source.zip")
	req.AddCookie(ck)
	req.RemoteAddr = "10.0.0.1:5555"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload = %d (%s)", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if archiveStore.appName != "demo" || archiveStore.filename != "source.zip" || archiveStore.digest != body["digest"] || !bytes.Equal(archiveStore.body, archive) {
		t.Fatalf("stored = %+v, response = %#v", archiveStore, body)
	}
	assertAudited(t, store, "app.source.upload", "success")
}

func TestUploadSourceArchiveRejectsTraversal(t *testing.T) {
	archive := zipWithFile(t, "../secret", "bad")
	store := newFakeStore()
	featureSet, _ := features.Parse([]string{string(features.SourceZip)})
	archiveStore := &fakeArchiveStore{}
	s := configuredAPIServer(t, store, featureSet, archiveStore, seedApp("demo"))
	ck := authedSession(t, store)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/apps/demo/source/archive", bytes.NewReader(archive))
	req.Header.Set("Content-Type", "application/zip")
	req.Header.Set("X-Orkano-Filename", "source.zip")
	req.AddCookie(ck)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "invalid_archive") {
		t.Fatalf("upload = %d (%s)", rec.Code, rec.Body.String())
	}
	if archiveStore.body != nil {
		t.Fatal("unsafe archive reached registry")
	}
}

var gitSourceForTest = orkanov1alpha1.GitSource{URL: "https://git.example.com/acme/app.git"}

func configuredAPIServer(t *testing.T, store *fakeStore, featureSet features.Set, archives ArchiveStore, objs ...client.Object) *Server {
	t.Helper()
	k8s := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&orkanov1alpha1.App{}, &orkanov1alpha1.Build{}).WithObjects(objs...).Build()
	s, err := New(Config{
		K8s: k8s, ViewerClient: k8s, PodLogs: &fakePodStreamer{}, DB: fakePinger{}, Store: store,
		Cipher: testCipherInstance, BootstrapTokenHash: auth.HashToken(testBootstrapToken), SPA: testSPA(), Now: fixedNow,
		Features: featureSet, Archives: archives,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func validSourceZIP(t *testing.T) []byte {
	return zipWithFile(t, "src/main.go", "package main\n")
}

func zipWithFile(t *testing.T, name, body string) []byte {
	t.Helper()
	var archive bytes.Buffer
	w := zip.NewWriter(&archive)
	f, err := w.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}
