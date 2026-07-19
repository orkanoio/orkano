package sourcefetch

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeDownloader struct {
	body []byte
}

func (f fakeDownloader) Download(_ context.Context, _, _ string, destination io.Writer) error {
	_, err := destination.Write(f.body)
	return err
}

func TestFetchExtractsVerifiedDownload(t *testing.T) {
	var archive bytes.Buffer
	w := zip.NewWriter(&archive)
	file, err := w.Create("src/main.go")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write([]byte("package main\n"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "source")
	if err := Fetch(context.Background(), fakeDownloader{body: archive.Bytes()}, Config{AppName: "demo", Digest: "sha256:" + strings.Repeat("0", 64), Destination: destination}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(destination, "src", "main.go"))
	if err != nil || string(body) != "package main\n" {
		t.Fatalf("main.go = %q, %v", body, err)
	}
}

func TestFetchRejectsNonEmptyDestination(t *testing.T) {
	destination := t.TempDir()
	if err := os.WriteFile(filepath.Join(destination, "existing"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Fetch(context.Background(), fakeDownloader{body: []byte("not used")}, Config{AppName: "demo", Digest: "sha256:" + strings.Repeat("0", 64), Destination: destination})
	if err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("Fetch error = %v", err)
	}
}
