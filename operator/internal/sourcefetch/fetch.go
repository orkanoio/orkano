package sourcefetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/orkanoio/orkano/internal/sourcearchive"
)

type Downloader interface {
	Download(ctx context.Context, appName, digest string, destination io.Writer) error
}

type Config struct {
	AppName     string
	Digest      string
	Destination string
}

func Fetch(ctx context.Context, downloader Downloader, cfg Config) error {
	if downloader == nil {
		return errors.New("source fetch: downloader is required")
	}
	if cfg.Destination == "" || !filepath.IsAbs(cfg.Destination) {
		return errors.New("source fetch: destination must be an absolute path")
	}
	if err := os.MkdirAll(cfg.Destination, 0o750); err != nil {
		return fmt.Errorf("source fetch: create destination: %w", err)
	}
	entries, err := os.ReadDir(cfg.Destination)
	if err != nil {
		return fmt.Errorf("source fetch: read destination: %w", err)
	}
	if len(entries) != 0 {
		return errors.New("source fetch: destination is not empty")
	}
	tmp, err := os.CreateTemp("", ".orkano-source-*.zip")
	if err != nil {
		return fmt.Errorf("source fetch: create temporary archive: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := downloader.Download(ctx, cfg.AppName, cfg.Digest, tmp); err != nil {
		return fmt.Errorf("source fetch: %w", errors.Join(err, tmp.Close()))
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("source fetch: close temporary archive: %w", err)
	}
	if err := sourcearchive.Extract(tmpName, cfg.Destination); err != nil {
		return fmt.Errorf("source fetch: extract archive: %w", err)
	}
	return nil
}
