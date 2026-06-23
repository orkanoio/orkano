package install

import (
	"context"
	"encoding/base64"
	"fmt"
	"path"
	"strings"
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
	// base64's alphabet contains no shell metacharacters, so the single-quoted
	// payload cannot break out of the command.
	cmd := fmt.Sprintf("%smkdir -p %s && printf %%s '%s' | base64 -d | %stee %s >/dev/null && %schmod %s %s",
		n.sudo, path.Dir(p), enc, n.sudo, p, n.sudo, mode, p)
	if err := n.runOK(ctx, cmd, "write "+p); err != nil {
		return false, err
	}
	return true, nil
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
