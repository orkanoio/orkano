//go:build imagepins

// The live half of the multi-arch image-pin guard: it shells out to
// `docker buildx imagetools inspect <ref> --raw` for every product image the
// operator hardcodes and asserts each is a multi-arch index. Build-tagged so it
// never runs in the normal suite (no registry traffic in `make test`); run it
// via `make verify-image-pins`, which CI runs as its own job. The refs come
// straight from the production constants, so a re-pin can't drift past the guard.
package imagepins_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/orkanoio/orkano/internal/doctor"
	"github.com/orkanoio/orkano/operator/internal/buildjob"
	"github.com/orkanoio/orkano/operator/internal/controller"
	"github.com/orkanoio/orkano/operator/internal/imagepins"
)

func TestProductImagePinsAreMultiArch(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not on PATH, skipping live image-pin check: %v", err)
	}
	if err := exec.Command("docker", "buildx", "version").Run(); err != nil {
		t.Skipf("docker buildx not available, skipping live image-pin check: %v", err)
	}
	refs := append([]string{buildjob.DefaultImage, buildjob.StaticServerImage, doctor.CanaryImage}, controller.PinnedPostgresImages()...)
	refs = append(refs, controller.PinnedMongoImages()...)
	for _, ref := range refs {
		t.Run(ref, func(t *testing.T) {
			raw, err := inspectRaw(t, ref)
			if err != nil {
				t.Fatalf("inspect %s: %v", ref, err)
			}
			if err := imagepins.VerifyMultiArch(raw); err != nil {
				t.Errorf("%s: %v", ref, err)
			}
		})
	}
}

// inspectRaw fetches the raw manifest for ref, retrying a few times to absorb a
// transient registry hiccup (the guard hits Docker Hub).
func inspectRaw(t *testing.T, ref string) ([]byte, error) {
	t.Helper()
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		out, err := exec.CommandContext(ctx, "docker", "buildx", "imagetools", "inspect", ref, "--raw").Output()
		cancel()
		if err == nil {
			return out, nil
		}
		lastErr = withStderr(err)
		t.Logf("attempt %d to inspect %s failed: %v", attempt, ref, lastErr)
		if attempt < 3 {
			time.Sleep(2 * time.Second)
		}
	}
	return nil, lastErr
}

// withStderr surfaces the captured stderr of a failed command: exec.Output()
// stores it on *exec.ExitError, but the default error string is only
// "exit status N" — useless for diagnosing a rate-limit or auth failure in CI.
func withStderr(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return fmt.Errorf("%w: %s", err, bytes.TrimSpace(ee.Stderr))
	}
	return err
}
