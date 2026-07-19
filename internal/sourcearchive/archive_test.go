package sourcearchive

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectAndExtract(t *testing.T) {
	archive := writeZip(t, []zipEntry{
		{name: "cmd/start.sh", body: "#!/bin/sh\necho ok\n", mode: 0o755},
		{name: "README.md", body: "hello", mode: 0o644},
	})
	info, err := Inspect(archive)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if info.Files != 2 || info.UncompressedBytes != uint64(len("#!/bin/sh\necho ok\nhello")) {
		t.Fatalf("info = %+v", info)
	}
	destination := t.TempDir()
	if err := Extract(archive, destination); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(destination, "cmd", "start.sh"))
	if err != nil || string(body) != "#!/bin/sh\necho ok\n" {
		t.Fatalf("start.sh = %q, %v", body, err)
	}
	stat, err := os.Stat(filepath.Join(destination, "cmd", "start.sh"))
	if err != nil || stat.Mode().Perm() != 0o755 {
		t.Fatalf("start.sh mode = %v, %v", stat.Mode().Perm(), err)
	}
}

func TestInspectRejectsUnsafeArchives(t *testing.T) {
	tests := []struct {
		name    string
		entries []zipEntry
		want    string
	}{
		{name: "traversal", entries: []zipEntry{{name: "../secret", body: "x"}}, want: "unsafe path"},
		{name: "absolute", entries: []zipEntry{{name: "/secret", body: "x"}}, want: "unsafe path"},
		{name: "backslash", entries: []zipEntry{{name: `dir\secret`, body: "x"}}, want: "unsafe path"},
		{name: "duplicate", entries: []zipEntry{{name: "a/../file", body: "x"}, {name: "file", body: "x"}}, want: "duplicate path"},
		{name: "symlink", entries: []zipEntry{{name: "link", body: "target", mode: os.ModeSymlink | 0o777}}, want: "not a regular file"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Inspect(writeZip(t, tc.entries))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Inspect error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestExtractRequiresEmptyDirectory(t *testing.T) {
	archive := writeZip(t, []zipEntry{{name: "file", body: "x"}})
	destination := t.TempDir()
	if err := os.WriteFile(filepath.Join(destination, "existing"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Extract(archive, destination); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("Extract error = %v", err)
	}
}

type zipEntry struct {
	name string
	body string
	mode os.FileMode
}

func writeZip(t *testing.T, entries []zipEntry) string {
	t.Helper()
	var body bytes.Buffer
	w := zip.NewWriter(&body)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		if entry.mode != 0 {
			header.SetMode(entry.mode)
		}
		part, err := w.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(t.TempDir(), "source.zip")
	if err := os.WriteFile(filename, body.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return filename
}
