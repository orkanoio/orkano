package preflight

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/orkanoio/orkano/api/check"
)

// sshReachableCheck proves Orkano can open an SSH session and run a command on
// the node. A connect, auth, or transport failure is a definitive "unreachable"
// — reported as a fail (so init refuses and the dependents block cleanly), not
// as a probe error (which would read as "the check itself broke").
func sshReachableCheck(opt Options) check.Check {
	return check.Check{
		ID:          IDSSHReachable,
		Severity:    check.SeverityCritical,
		Summary:     "SSH connection to the node succeeds",
		Remediation: "check the address and that sshd is running, and that the key authenticates: ssh " + opt.target(),
		Probe: func(ctx context.Context) (check.Result, error) {
			res, err := opt.Executor.Run(ctx, "true")
			if err != nil {
				return check.Result{
					Status:  check.StatusFail,
					Message: fmt.Sprintf("cannot reach %s over SSH: %v", opt.target(), err),
				}, nil
			}
			if res.ExitStatus != 0 {
				return check.Result{
					Status:  check.StatusFail,
					Message: fmt.Sprintf("connected to %s but a trivial command exited %d: %s", opt.target(), res.ExitStatus, firstLine(res.Stderr)),
				}, nil
			}
			return check.Result{Status: check.StatusPass, Message: "connected to " + opt.target() + " over SSH"}, nil
		},
	}
}

// archSupportedCheck reads the node's machine architecture: Orkano publishes
// images for amd64 and arm64 only, so anything else cannot run the platform.
func archSupportedCheck(opt Options) check.Check {
	return check.Check{
		ID:          IDArchSupported,
		Severity:    check.SeverityCritical,
		Summary:     "node CPU architecture is amd64 or arm64",
		Remediation: "Orkano publishes images for amd64 and arm64 only; use a node of one of those architectures",
		Requires:    []string{IDSSHReachable},
		Probe: func(ctx context.Context) (check.Result, error) {
			res, err := opt.Executor.Run(ctx, "uname -m")
			if err != nil {
				return check.Result{}, fmt.Errorf("run uname: %w", err)
			}
			if res.ExitStatus != 0 {
				return check.Result{}, fmt.Errorf("uname -m exited %d: %s", res.ExitStatus, firstLine(res.Stderr))
			}
			machine := strings.TrimSpace(res.Stdout)
			switch machine {
			case "x86_64", "amd64":
				return check.Result{Status: check.StatusPass, Message: "architecture " + machine + " (amd64)"}, nil
			case "aarch64", "arm64":
				return check.Result{Status: check.StatusPass, Message: "architecture " + machine + " (arm64)"}, nil
			case "":
				return check.Result{}, errors.New("uname -m returned no output")
			default:
				return check.Result{
					Status:  check.StatusFail,
					Message: "unsupported architecture " + machine + "; Orkano supports amd64 and arm64",
				}, nil
			}
		},
	}
}

// portsFreeCheck lists the node's listening TCP sockets and reports any of the
// required ports already in use. It reads the live kernel socket table (via ss)
// rather than a config file — the port is occupied if and only if something is
// actually listening on it.
func portsFreeCheck(opt Options) check.Check {
	ports := opt.ports()
	return check.Check{
		ID:          IDPortsFree,
		Severity:    check.SeverityCritical,
		Summary:     "ports required by k3s and the ingress are free",
		Remediation: "stop whatever is listening on the reported ports, or pick a clean node",
		Requires:    []string{IDSSHReachable},
		Probe: func(ctx context.Context) (check.Result, error) {
			res, err := opt.Executor.Run(ctx, "ss -Hltn")
			if err != nil {
				return check.Result{}, fmt.Errorf("run ss: %w", err)
			}
			if res.ExitStatus != 0 {
				return check.Result{}, fmt.Errorf("ss -Hltn exited %d (is iproute2 installed?): %s", res.ExitStatus, firstLine(res.Stderr))
			}
			listening := listeningPorts(res.Stdout)
			var occupied []int
			for _, p := range ports {
				if _, ok := listening[p]; ok {
					occupied = append(occupied, p)
				}
			}
			if len(occupied) > 0 {
				return check.Result{
					Status:  check.StatusFail,
					Message: "ports already in use: " + joinInts(occupied),
				}, nil
			}
			return check.Result{Status: check.StatusPass, Message: "required ports free: " + joinInts(ports)}, nil
		},
	}
}

// timeSyncedCheck measures the node clock against the control host. A skewed
// clock breaks time-dependent machinery (Let's Encrypt validation, etcd raft,
// short-lived tokens), but NTP usually converges, so it warns rather than
// refusing the install.
func timeSyncedCheck(opt Options) check.Check {
	maxSkew := opt.maxSkew()
	return check.Check{
		ID:          IDTimeSynced,
		Severity:    check.SeverityWarning,
		Summary:     "node clock is within tolerance of the control host",
		Remediation: "enable time sync on the node, e.g. `timedatectl set-ntp true` or install chrony",
		Requires:    []string{IDSSHReachable},
		Probe: func(ctx context.Context) (check.Result, error) {
			before := opt.now()
			res, err := opt.Executor.Run(ctx, "date -u +%s")
			after := opt.now()
			if err != nil {
				return check.Result{}, fmt.Errorf("run date: %w", err)
			}
			if res.ExitStatus != 0 {
				return check.Result{}, fmt.Errorf("date exited %d: %s", res.ExitStatus, firstLine(res.Stderr))
			}
			text := strings.TrimSpace(res.Stdout)
			epoch, err := strconv.ParseInt(text, 10, 64)
			if err != nil {
				return check.Result{}, fmt.Errorf("parse node time %q: %w", text, err)
			}
			// The node clock was sampled somewhere in [before, after]; the midpoint
			// is the control reference, so the SSH round-trip roughly cancels out.
			mid := before.Add(after.Sub(before) / 2)
			offset := time.Unix(epoch, 0).Sub(mid)
			if abs(offset) > maxSkew {
				return check.Result{
					Status:  check.StatusFail,
					Message: fmt.Sprintf("node clock is ~%s %s the control host (tolerance %s)", abs(offset).Round(time.Second), direction(offset), maxSkew),
				}, nil
			}
			return check.Result{Status: check.StatusPass, Message: "node clock within " + maxSkew.String() + " of the control host"}, nil
		},
	}
}

// listeningPorts extracts the set of local TCP ports from `ss -Hltn` output.
// The local address is the fourth column; the port is the text after its last
// colon, correct for 0.0.0.0:p, *:p, [::]:p and addr:p alike. Lines that do not
// parse are skipped — a partial parse must not be mistaken for "no listeners".
func listeningPorts(ssOutput string) map[int]struct{} {
	out := make(map[int]struct{})
	for _, line := range strings.Split(ssOutput, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		local := fields[3]
		i := strings.LastIndex(local, ":")
		if i < 0 {
			continue
		}
		port, err := strconv.Atoi(local[i+1:])
		if err != nil {
			continue
		}
		out[port] = struct{}{}
	}
	return out
}

func joinInts(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = strconv.Itoa(x)
	}
	return strings.Join(parts, ", ")
}

func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func direction(offset time.Duration) string {
	if offset < 0 {
		return "behind"
	}
	return "ahead of"
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
