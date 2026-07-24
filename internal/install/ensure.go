package install

import (
	"context"
	"encoding/base64"
	"fmt"
	"path"
	"strings"
)

const (
	// maxInlineBase64 bounds a single-command base64 write. Linux execve caps one
	// argument string at MAX_ARG_STRLEN (128 KiB), so a larger payload would fail
	// with E2BIG. The cap is a kernel limit, not an sshd artifact — it applies
	// identically on both transports: sshd runs the remote command as
	// `sh -c '<string>'`, and localexec passes it as one argv to a local sh. Files
	// above it (the ~1 MB vendored cert-manager manifest) are written in chunks to
	// a temp file, then atomically renamed. Both bounds are multiples of 4 so each
	// base64 chunk decodes to whole bytes independently.
	maxInlineBase64 = 96 * 1024
	chunkBase64     = 48 * 1024
)

// node wraps a Runner with the sudo prefix and a logger, providing the
// idempotent file write Apply relies on (mirrors internal/k3s's ensureFile —
// deliberately duplicated rather than coupling the two packages).
type node struct {
	r    Runner
	sudo string
	logf func(format string, args ...any)
}

func newNode(r Runner, sudo bool, logf func(format string, args ...any)) *node {
	prefix := ""
	if sudo {
		prefix = "sudo "
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &node{r: r, sudo: prefix, logf: logf}
}

// ensureFile writes content to p with mode only when the node's current
// contents differ, reporting whether it wrote. Parent directories are created.
func (n *node) ensureFile(ctx context.Context, p string, content []byte, mode string) (bool, error) {
	cur, err := n.r.Run(ctx, n.sudo+"cat "+p)
	if err != nil {
		return false, fmt.Errorf("install: read %s: %w", p, err)
	}
	if cur.ExitStatus == 0 && cur.Stdout == string(content) {
		return false, nil
	}

	enc := base64.StdEncoding.EncodeToString(content)
	if len(enc) <= maxInlineBase64 {
		if err := n.writeInline(ctx, p, enc); err != nil {
			return false, err
		}
	} else if err := n.writeChunked(ctx, p, enc); err != nil {
		return false, err
	}
	if err := n.runOK(ctx, fmt.Sprintf("%schmod %s %s", n.sudo, mode, p), "chmod "+p); err != nil {
		return false, err
	}
	return true, nil
}

// migrateLegacyManifests renames the v0.1.0 filenames whose dots and
// underscores collapsed all Orkano CRDs onto one k3s AddOn identity. Removing
// an auto-deploy source file does not remove its resources; the subsequent
// explicit CRD apply converges them under the unique replacement filenames.
func (n *node) migrateLegacyManifests(ctx context.Context, base string, files []manifestFile) (bool, error) {
	changed := false
	for _, f := range files {
		if f.legacyName == "" {
			continue
		}

		legacyPath := path.Join(base, f.legacyName)
		legacy, err := n.r.Run(ctx, n.sudo+"cat "+legacyPath)
		if err != nil {
			return false, fmt.Errorf("install: read legacy manifest %s: %w", legacyPath, err)
		}
		if legacy.ExitStatus != 0 {
			continue
		}

		currentPath := path.Join(base, f.name)
		current, err := n.r.Run(ctx, n.sudo+"cat "+currentPath)
		if err != nil {
			return false, fmt.Errorf("install: read replacement manifest %s: %w", currentPath, err)
		}

		var cmd string
		if current.ExitStatus == 0 {
			cmd = fmt.Sprintf("%srm -f %s", n.sudo, legacyPath)
		} else {
			cmd = fmt.Sprintf("%smv %s %s", n.sudo, legacyPath, currentPath)
		}
		if err := n.runOK(ctx, cmd, "migrate legacy manifest "+f.legacyName); err != nil {
			return false, err
		}
		n.logf("migrated %s to %s", f.legacyName, f.name)
		changed = true
	}
	return changed, nil
}

// writeInline writes a small payload in one command. base64's alphabet has no
// shell metacharacters, so the single-quoted payload cannot break out.
//
// It writes to PATH.tmp and renames, for the same reason writeChunked does:
// `tee` opens its target with O_TRUNC, and k3s re-reads a manifest whenever its
// mtime changes. A poll landing inside the truncate-then-write window would
// read an empty document, apply an empty object set under that AddOn's owner,
// and prune everything the AddOn had created — for a CRD file, deleting the CRD
// and cascading to every custom resource. rename(2) is atomic, so k3s can only
// ever observe the old file or the new one.
func (n *node) writeInline(ctx context.Context, p, enc string) error {
	tmp := p + ".tmp"
	cmd := fmt.Sprintf("%smkdir -p %s && printf %%s '%s' | base64 -d | %stee %s >/dev/null",
		n.sudo, path.Dir(p), enc, n.sudo, tmp)
	if err := n.runOK(ctx, cmd, "write "+p); err != nil {
		return err
	}
	return n.runOK(ctx, fmt.Sprintf("%smv %s %s", n.sudo, tmp, p), "finalize "+p)
}

// writeChunked writes a large payload as a sequence of base64 chunks appended to
// a temp file, then renames it over the destination atomically — so a partial
// write can never be observed as the real file, and each command stays well
// under MAX_ARG_STRLEN.
func (n *node) writeChunked(ctx context.Context, p, enc string) error {
	tmp := p + ".tmp"
	first := true
	for i := 0; i < len(enc); i += chunkBase64 {
		end := min(i+chunkBase64, len(enc))
		redirect := n.sudo + "tee -a " + tmp
		prefix := ""
		if first {
			// First chunk truncates/creates the temp file (no -a) after mkdir.
			redirect = n.sudo + "tee " + tmp
			prefix = n.sudo + "mkdir -p " + path.Dir(p) + " && "
			first = false
		}
		cmd := fmt.Sprintf("%sprintf %%s '%s' | base64 -d | %s >/dev/null", prefix, enc[i:end], redirect)
		if err := n.runOK(ctx, cmd, "write chunk of "+p); err != nil {
			return err
		}
	}
	return n.runOK(ctx, fmt.Sprintf("%smv %s %s", n.sudo, tmp, p), "finalize "+p)
}

// runOK runs cmd and returns an error if it fails to run or exits non-zero.
func (n *node) runOK(ctx context.Context, cmd, desc string) error {
	res, err := n.r.Run(ctx, cmd)
	if err != nil {
		return fmt.Errorf("install: %s: %w", desc, err)
	}
	if res.ExitStatus != 0 {
		return fmt.Errorf("install: %s exited %d: %s", desc, res.ExitStatus, firstLine(res.Stderr))
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
