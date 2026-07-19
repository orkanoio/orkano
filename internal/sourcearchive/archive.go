package sourcearchive

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	MaxCompressedBytes   int64  = 32 << 20
	MaxUncompressedBytes uint64 = 256 << 20
	MaxFiles                    = 10_000
	MaxCompressionRatio  uint64 = 200
)

type Info struct {
	Files             int
	UncompressedBytes uint64
}

func Inspect(filename string) (Info, error) {
	r, err := zip.OpenReader(filename)
	if err != nil {
		return Info{}, fmt.Errorf("open ZIP: %w", err)
	}
	defer func() { _ = r.Close() }()

	if len(r.File) == 0 {
		return Info{}, errors.New("ZIP contains no files")
	}
	if len(r.File) > MaxFiles {
		return Info{}, fmt.Errorf("ZIP contains %d entries; maximum is %d", len(r.File), MaxFiles)
	}

	seen := make(map[string]struct{}, len(r.File))
	var info Info
	for _, file := range r.File {
		clean, err := safePath(file.Name)
		if err != nil {
			return Info{}, err
		}
		if _, ok := seen[clean]; ok {
			return Info{}, fmt.Errorf("ZIP contains duplicate path %q", clean)
		}
		seen[clean] = struct{}{}
		if file.Flags&0x1 != 0 {
			return Info{}, fmt.Errorf("ZIP entry %q is encrypted", clean)
		}
		if file.Method != zip.Store && file.Method != zip.Deflate {
			return Info{}, fmt.Errorf("ZIP entry %q uses unsupported compression method %d", clean, file.Method)
		}
		mode := file.Mode()
		if !mode.IsDir() && !mode.IsRegular() {
			return Info{}, fmt.Errorf("ZIP entry %q is not a regular file or directory", clean)
		}
		if mode.IsDir() {
			continue
		}
		if file.UncompressedSize64 > MaxUncompressedBytes-info.UncompressedBytes {
			return Info{}, fmt.Errorf("ZIP expands beyond the %d-byte limit", MaxUncompressedBytes)
		}
		if file.UncompressedSize64 > 1<<20 && file.UncompressedSize64 > (file.CompressedSize64+1)*MaxCompressionRatio {
			return Info{}, fmt.Errorf("ZIP entry %q exceeds the maximum compression ratio", clean)
		}
		info.Files++
		info.UncompressedBytes += file.UncompressedSize64
	}
	if info.Files == 0 {
		return Info{}, errors.New("ZIP contains no regular files")
	}
	return info, nil
}

func Extract(filename, destination string) error {
	if _, err := Inspect(filename); err != nil {
		return err
	}
	entries, err := os.ReadDir(destination)
	if err != nil {
		return fmt.Errorf("read extraction directory: %w", err)
	}
	if len(entries) != 0 {
		return errors.New("extraction directory is not empty")
	}

	r, err := zip.OpenReader(filename)
	if err != nil {
		return fmt.Errorf("open ZIP: %w", err)
	}
	defer func() { _ = r.Close() }()

	var extracted uint64
	for _, archived := range r.File {
		clean, err := safePath(archived.Name)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, filepath.FromSlash(clean))
		if archived.Mode().IsDir() {
			if err := os.MkdirAll(target, 0o750); err != nil {
				return fmt.Errorf("create directory %q: %w", clean, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return fmt.Errorf("create parent for %q: %w", clean, err)
		}
		in, err := archived.Open()
		if err != nil {
			return fmt.Errorf("open ZIP entry %q: %w", clean, err)
		}
		mode := os.FileMode(0o644)
		if archived.Mode().Perm()&0o111 != 0 {
			mode = 0o755
		}
		// target is rooted in the empty destination and every archive path was
		// rejected unless safePath proved it relative and traversal-free.
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode) //nolint:gosec
		if err != nil {
			return fmt.Errorf("create extracted file %q: %w", clean, errors.Join(err, in.Close()))
		}
		remaining := int64(MaxUncompressedBytes - extracted)
		written, copyErr := io.Copy(out, io.LimitReader(in, remaining+1))
		closeErr := errors.Join(in.Close(), out.Close())
		if copyErr != nil || closeErr != nil {
			return fmt.Errorf("extract ZIP entry %q: %w", clean, errors.Join(copyErr, closeErr))
		}
		if written < 0 || written > remaining {
			return fmt.Errorf("ZIP entry %q exceeded its declared or total size", clean)
		}
		writtenSize := uint64(written)
		if writtenSize != archived.UncompressedSize64 {
			return fmt.Errorf("ZIP entry %q exceeded its declared or total size", clean)
		}
		extracted += writtenSize
	}
	return nil
}

func safePath(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, '\x00') || strings.Contains(name, `\`) || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("ZIP contains unsafe path %q", name)
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("ZIP contains unsafe path %q", name)
	}
	return clean, nil
}
