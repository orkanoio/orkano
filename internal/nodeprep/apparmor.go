// Package nodeprep performs the per-node host preparation orkano init runs over
// SSH, alongside the k3s bootstrap: it installs and loads the orkano-buildkit
// AppArmor profile that build pods are confined by (ADR-0012), and ships the
// build.apparmor-profile-loaded check that verifies it.
//
// The profile is load-bearing. Build pods reference it via
// securityContext.appArmorProfile Localhost/orkano-buildkit; without it loaded
// in enforce mode on a node, a build pod scheduled there cannot start, or — with
// containerd's default profile, which denies mount(2) silently — hits a mount
// denial with no audit entry. So the load is verified, never assumed: a node
// where the profile cannot be loaded is one where every build silently fails,
// and EnsureAppArmorProfile refuses rather than leaving that latent.
package nodeprep

import (
	"context"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/orkanoio/orkano/api/check"
	"github.com/orkanoio/orkano/internal/ssh"
)

// apparmorProfile is the canonical orkano-buildkit profile, embedded so the CLI
// is self-contained. It is kept byte-identical to
// config/apparmor/orkano-buildkit.profile (the live copy consumed by the build
// Job securityContext and the substrate smoke) by the package's drift test.
//
//go:embed orkano-buildkit.profile
var apparmorProfile []byte

// ProfileName is the AppArmor profile name build pods reference via
// securityContext.appArmorProfile Localhost/orkano-buildkit.
const ProfileName = "orkano-buildkit"

// IDAppArmorProfileLoaded is the permanent ID of the check that verifies the
// profile is loaded in enforce mode. It appears in --json output and CI configs
// (see api/check.Check.ID), so it does not change once shipped.
const IDAppArmorProfileLoaded = "build.apparmor-profile-loaded"

const (
	// profilePath is where the profile is installed. /etc/apparmor.d is the
	// directory the apparmor systemd service loads on boot, so placing it there
	// also makes the load survive a reboot.
	profilePath = "/etc/apparmor.d/" + ProfileName
	// profilesFile is the kernel's live list of loaded profiles and their modes.
	// It is the capability source the check reads — what is actually loaded, not
	// what a config file claims.
	profilesFile = "/sys/kernel/security/apparmor/profiles"
	// modeEnforce is the only mode that actually confines: complain logs but
	// allows everything, defeating the profile's purpose.
	modeEnforce = "enforce"
)

// Runner runs a command on the target node. *ssh.Client satisfies it; tests
// supply a fake. The reuse of ssh.Result mirrors internal/k3s and
// internal/preflight.
type Runner interface {
	Run(ctx context.Context, cmd string) (ssh.Result, error)
}

// Options parameterises EnsureAppArmorProfile.
type Options struct {
	// Runner reaches the node. Required.
	Runner Runner
	// Sudo prefixes privileged commands with sudo. Writing under /etc/apparmor.d
	// and running apparmor_parser need root, and reading the kernel profile list
	// under securityfs typically does too; set it whenever the SSH user is not
	// root (the user must then have passwordless sudo).
	Sudo bool
	// Logf receives human-readable progress lines; nil discards them.
	Logf func(format string, args ...any)
}

// Result reports what EnsureAppArmorProfile found and did.
type Result struct {
	// Changed is true when this run wrote the profile file or (re)loaded it into
	// the kernel.
	Changed bool
	// Mode is the verified load mode of the profile after the run ("enforce").
	Mode string
}

// EnsureAppArmorProfile installs the orkano-buildkit AppArmor profile to
// /etc/apparmor.d and loads it into the kernel in enforce mode, then verifies
// the load. It is idempotent: a re-run on a node that already has the profile
// loaded in enforce mode writes nothing and reloads nothing.
func EnsureAppArmorProfile(ctx context.Context, opt Options) (*Result, error) {
	if opt.Runner == nil {
		return nil, errors.New("nodeprep: runner is required")
	}
	p := &prep{opt: opt}
	if opt.Sudo {
		p.sudo = "sudo "
	}

	res := &Result{}

	wrote, err := p.ensureFile(ctx, profilePath, apparmorProfile, "0644")
	if err != nil {
		return nil, err
	}

	mode, loaded, err := p.queryProfile(ctx)
	if err != nil {
		return nil, err
	}

	// Reload when the file changed, the profile is not loaded at all, or it is
	// loaded but not enforcing. apparmor_parser -r is a replace, so reloading an
	// identical profile would be harmless; gating on these conditions only keeps a
	// true no-op re-run from reporting Changed.
	if wrote || !loaded || mode != modeEnforce {
		p.logf("loading AppArmor profile %s", ProfileName)
		if err := p.runOK(ctx, p.sudo+"apparmor_parser -r "+profilePath, "load AppArmor profile"); err != nil {
			return nil, err
		}
		res.Changed = true
		if mode, loaded, err = p.queryProfile(ctx); err != nil {
			return nil, err
		}
	}

	if !loaded {
		return nil, fmt.Errorf("nodeprep: AppArmor profile %s is not loaded after apparmor_parser; is AppArmor enabled on this node?", ProfileName)
	}
	if mode != modeEnforce {
		return nil, fmt.Errorf("nodeprep: AppArmor profile %s loaded in %q mode, want enforce", ProfileName, mode)
	}
	res.Mode = mode
	p.logf("AppArmor profile %s loaded (%s)", ProfileName, mode)
	return res, nil
}

// AppArmorProfileLoadedCheck verifies the orkano-buildkit AppArmor profile is
// loaded in enforce mode — the confinement build pods depend on (ADR-0012). It
// reads the kernel's live profile list rather than any config file, so it
// reports what is actually loaded. It is shipped for orkano doctor (Phase 3) and
// used by orkano init to verify the load it just performed.
//
// sudo prefixes the profile-list read; reading the list under securityfs
// typically needs root, so pass true when the SSH user is not root.
//
// The result mapping upholds the runner's "unknown never hardened" invariant: a
// read that could not be determined — a transport failure, or a non-zero exit
// that cannot distinguish "AppArmor disabled" from "securityfs unreadable" — is
// a probe ERROR, while a definitively bad but known state (the profile absent,
// or loaded in complain mode) is a StatusFail.
func AppArmorProfileLoadedCheck(r Runner, sudo bool) check.Check {
	prefix := ""
	if sudo {
		prefix = "sudo "
	}
	return check.Check{
		ID:          IDAppArmorProfileLoaded,
		Severity:    check.SeverityCritical,
		Summary:     "orkano-buildkit AppArmor profile is loaded in enforce mode",
		Remediation: "run `orkano init` to install and load it, or load it manually: install the profile to " + profilePath + " and run `apparmor_parser -r " + profilePath + "`",
		Probe: func(ctx context.Context) (check.Result, error) {
			res, err := r.Run(ctx, prefix+"cat "+profilesFile)
			if err != nil {
				return check.Result{}, fmt.Errorf("read AppArmor profiles: %w", err)
			}
			if res.ExitStatus != 0 {
				return check.Result{}, fmt.Errorf("cannot read %s (is AppArmor enabled and securityfs readable?): %s", profilesFile, firstLine(res.Stderr))
			}
			mode, loaded := profileMode(res.Stdout, ProfileName)
			switch {
			case !loaded:
				return check.Result{
					Status:  check.StatusFail,
					Message: fmt.Sprintf("AppArmor profile %s is not loaded; build pods confined by it cannot start", ProfileName),
				}, nil
			case mode != modeEnforce:
				return check.Result{
					Status:  check.StatusFail,
					Message: fmt.Sprintf("AppArmor profile %s is loaded but in %q mode, not enforce", ProfileName, mode),
				}, nil
			default:
				return check.Result{Status: check.StatusPass, Message: fmt.Sprintf("AppArmor profile %s loaded (enforce)", ProfileName)}, nil
			}
		},
	}
}

type prep struct {
	opt  Options
	sudo string
}

func (p *prep) logf(format string, args ...any) {
	if p.opt.Logf != nil {
		p.opt.Logf(format, args...)
	}
}

// queryProfile reads the kernel's loaded-profile list and reports the mode of
// the orkano-buildkit profile. A read failure (AppArmor disabled, or the
// securityfs not mounted) is an error, distinct from the profile simply not
// being present yet (loaded == false).
func (p *prep) queryProfile(ctx context.Context) (mode string, loaded bool, err error) {
	res, err := p.opt.Runner.Run(ctx, p.sudo+"cat "+profilesFile)
	if err != nil {
		return "", false, fmt.Errorf("nodeprep: read AppArmor profiles: %w", err)
	}
	if res.ExitStatus != 0 {
		return "", false, fmt.Errorf("nodeprep: cannot read %s (is AppArmor enabled on this node?): %s", profilesFile, firstLine(res.Stderr))
	}
	mode, loaded = profileMode(res.Stdout, ProfileName)
	return mode, loaded, nil
}

// ensureFile writes content to path with mode only when the node's current
// contents differ, reporting whether it wrote. Parent directories are created.
// It mirrors internal/k3s's helper of the same name; base64's alphabet has no
// shell metacharacters, so the single-quoted payload cannot break out of the
// command.
func (p *prep) ensureFile(ctx context.Context, path string, content []byte, mode string) (bool, error) {
	cur, err := p.opt.Runner.Run(ctx, p.sudo+"cat "+path)
	if err != nil {
		return false, fmt.Errorf("nodeprep: read %s: %w", path, err)
	}
	if cur.ExitStatus == 0 && cur.Stdout == string(content) {
		return false, nil
	}

	dir := path[:strings.LastIndex(path, "/")]
	enc := base64.StdEncoding.EncodeToString(content)
	cmd := fmt.Sprintf("%smkdir -p %s && printf %%s '%s' | base64 -d | %stee %s >/dev/null && %schmod %s %s",
		p.sudo, dir, enc, p.sudo, path, p.sudo, mode, path)
	if err := p.runOK(ctx, cmd, "write "+path); err != nil {
		return false, err
	}
	p.logf("wrote %s", path)
	return true, nil
}

// runOK runs cmd and turns a transport error or a non-zero exit into a Go error;
// it is for commands that must succeed (the write, the load).
func (p *prep) runOK(ctx context.Context, cmd, desc string) error {
	res, err := p.opt.Runner.Run(ctx, cmd)
	if err != nil {
		return fmt.Errorf("nodeprep: %s: %w", desc, err)
	}
	if res.ExitStatus != 0 {
		return fmt.Errorf("nodeprep: %s exited %d: %s", desc, res.ExitStatus, firstLine(res.Stderr))
	}
	return nil
}

// profileMode finds the named profile in `cat /sys/kernel/security/apparmor/profiles`
// output and returns its mode. Lines have the form "name (mode)"; an AppArmor
// profile name can itself contain spaces, so the mode is the final parenthesised
// field and the name is everything before it.
func profileMode(list, name string) (mode string, loaded bool) {
	for _, line := range strings.Split(list, "\n") {
		line = strings.TrimSpace(line)
		open := strings.LastIndex(line, " (")
		if open < 0 || !strings.HasSuffix(line, ")") {
			continue
		}
		if line[:open] != name {
			continue
		}
		return line[open+2 : len(line)-1], true
	}
	return "", false
}

// firstLine returns the first non-empty line of s, trimmed — command stderr is
// often multi-line and only the first line is useful in a one-line message.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}
