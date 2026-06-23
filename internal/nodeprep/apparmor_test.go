package nodeprep

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/orkanoio/orkano/api/check"
	"github.com/orkanoio/orkano/internal/ssh"
)

// fakeNode is a scriptable AppArmor node: it tracks the installed profile file
// and the kernel load state, records every command, and lets a test seed the
// initial state and inject transport/exit failures.
type fakeNode struct {
	mu   sync.Mutex
	cmds []string

	fileContent string // current /etc/apparmor.d/orkano-buildkit content; "" = absent
	fileExists  bool
	loaded      bool   // orkano-buildkit present in the kernel list
	mode        string // its mode when loaded
	otherLoaded string // an unrelated profile always present in the list

	// failures
	profilesExit      int    // exit code for `cat <profilesFile>` (0 = ok)
	profilesCalls     int    // number of `cat <profilesFile>` reads so far
	profilesErrOnCall int    // if >0, the Nth profiles read returns a transport error
	parserExit        int    // exit code for `apparmor_parser -r` (0 = ok)
	parserErr         error  // if set, apparmor_parser returns this transport error
	parserNoLoad      bool   // apparmor_parser exits 0 but does NOT load the profile
	parserMode        string // mode apparmor_parser loads the profile in (default enforce)
	runErrOn          string // substring; a matching command returns transport error
}

func (n *fakeNode) Run(_ context.Context, raw string) (ssh.Result, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.cmds = append(n.cmds, raw)
	cmd := strings.ReplaceAll(raw, "sudo ", "")

	if n.runErrOn != "" && strings.Contains(cmd, n.runErrOn) {
		return ssh.Result{}, errors.New("transport down")
	}

	switch {
	case strings.Contains(cmd, "apparmor_parser -r"):
		if n.parserErr != nil {
			return ssh.Result{}, n.parserErr
		}
		if n.parserExit != 0 {
			return ssh.Result{ExitStatus: n.parserExit, Stderr: "apparmor_parser: failed\n"}, nil
		}
		n.fileExists = true
		if !n.parserNoLoad {
			n.loaded = true
			n.mode = modeEnforce // our profile carries no complain flag
			if n.parserMode != "" {
				n.mode = n.parserMode
			}
		}
		return ssh.Result{}, nil

	case cmd == "cat "+profilesFile:
		n.profilesCalls++
		if n.profilesErrOnCall != 0 && n.profilesCalls == n.profilesErrOnCall {
			return ssh.Result{}, errors.New("transport down")
		}
		if n.profilesExit != 0 {
			return ssh.Result{ExitStatus: n.profilesExit, Stderr: "cat: " + profilesFile + ": No such file or directory\n"}, nil
		}
		var b strings.Builder
		if n.otherLoaded != "" {
			b.WriteString(n.otherLoaded + " (enforce)\n")
		}
		if n.loaded {
			b.WriteString(ProfileName + " (" + n.mode + ")\n")
		}
		return ssh.Result{Stdout: b.String()}, nil

	case strings.Contains(cmd, "| base64 -d |"):
		path, content := decodeWrite(cmd)
		if path == profilePath {
			n.fileContent = content
			n.fileExists = true
		}
		return ssh.Result{}, nil

	case strings.HasPrefix(cmd, "cat "):
		path := strings.TrimPrefix(cmd, "cat ")
		if path == profilePath && n.fileExists {
			return ssh.Result{Stdout: n.fileContent}, nil
		}
		return ssh.Result{ExitStatus: 1, Stderr: "cat: " + path + ": No such file or directory\n"}, nil

	default:
		return ssh.Result{ExitStatus: 127, Stderr: "unexpected: " + cmd + "\n"}, nil
	}
}

func (n *fakeNode) ran(substr string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, c := range n.cmds {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

// decodeWrite extracts the path and decoded content from an ensureFile write
// command (`printf %s 'BASE64' | base64 -d | tee PATH >/dev/null && ...`).
func decodeWrite(cmd string) (path, content string) {
	const start = "printf %s '"
	i := strings.Index(cmd, start)
	j := strings.Index(cmd, "' | base64 -d")
	if i < 0 || j < 0 {
		return "", ""
	}
	dec, _ := base64.StdEncoding.DecodeString(cmd[i+len(start) : j])
	const teeMark = "tee "
	rest := cmd[strings.Index(cmd, teeMark)+len(teeMark):]
	return strings.TrimSpace(rest[:strings.Index(rest, " >/dev/null")]), string(dec)
}

func TestEnsureFreshNode(t *testing.T) {
	n := &fakeNode{otherLoaded: "cri-containerd.apparmor.d"}
	res, err := EnsureAppArmorProfile(context.Background(), Options{Runner: n, Sudo: true})
	if err != nil {
		t.Fatalf("EnsureAppArmorProfile: %v", err)
	}
	if !res.Changed || res.Mode != modeEnforce {
		t.Errorf("fresh install: got %+v, want Changed=true Mode=enforce", res)
	}
	if !n.ran("apparmor_parser -r " + profilePath) {
		t.Error("profile was not loaded")
	}
	// The write must carry the embedded profile, sudo-prefixed.
	if !n.ran("sudo tee " + profilePath) {
		t.Error("profile file was not written with sudo")
	}
	if n.fileContent != string(apparmorProfile) {
		t.Errorf("written profile content does not match the embedded profile:\n%s", n.fileContent)
	}
}

func TestEnsureIdempotent(t *testing.T) {
	// Profile already present and loaded in enforce: a no-op re-run.
	n := &fakeNode{
		fileExists:  true,
		fileContent: string(apparmorProfile),
		loaded:      true,
		mode:        modeEnforce,
		otherLoaded: "cri-containerd.apparmor.d",
	}
	res, err := EnsureAppArmorProfile(context.Background(), Options{Runner: n})
	if err != nil {
		t.Fatalf("EnsureAppArmorProfile: %v", err)
	}
	if res.Changed {
		t.Error("idempotent re-run reported Changed")
	}
	if res.Mode != modeEnforce {
		t.Errorf("Mode = %q, want enforce", res.Mode)
	}
	if n.ran("apparmor_parser") {
		t.Error("idempotent re-run reloaded the profile")
	}
	if n.ran("| base64 -d |") {
		t.Error("idempotent re-run rewrote the profile file")
	}
}

func TestEnsureContentDrift(t *testing.T) {
	// File present but stale: rewrite and reload.
	n := &fakeNode{
		fileExists:  true,
		fileContent: "# an older profile\n",
		loaded:      true,
		mode:        modeEnforce,
	}
	res, err := EnsureAppArmorProfile(context.Background(), Options{Runner: n})
	if err != nil {
		t.Fatalf("EnsureAppArmorProfile: %v", err)
	}
	if !res.Changed {
		t.Error("content drift should report Changed")
	}
	if !n.ran("| base64 -d |") || !n.ran("apparmor_parser") {
		t.Error("drift should rewrite and reload")
	}
	if n.fileContent != string(apparmorProfile) {
		t.Error("drifted profile not overwritten with the embedded content")
	}
}

func TestEnsureFilePresentNotLoaded(t *testing.T) {
	// Correct file on disk but not in the kernel (e.g. never parsed): no rewrite,
	// but a reload is still required.
	n := &fakeNode{
		fileExists:  true,
		fileContent: string(apparmorProfile),
		loaded:      false,
	}
	res, err := EnsureAppArmorProfile(context.Background(), Options{Runner: n})
	if err != nil {
		t.Fatalf("EnsureAppArmorProfile: %v", err)
	}
	if !res.Changed {
		t.Error("loading an unloaded profile should report Changed")
	}
	if n.ran("| base64 -d |") {
		t.Error("an up-to-date file should not be rewritten")
	}
	if !n.ran("apparmor_parser") {
		t.Error("an unloaded profile should be loaded")
	}
}

func TestEnsureComplainModeReloadedToEnforce(t *testing.T) {
	// Loaded but in complain mode: reload puts it back to enforce.
	n := &fakeNode{
		fileExists:  true,
		fileContent: string(apparmorProfile),
		loaded:      true,
		mode:        "complain",
	}
	res, err := EnsureAppArmorProfile(context.Background(), Options{Runner: n})
	if err != nil {
		t.Fatalf("EnsureAppArmorProfile: %v", err)
	}
	if !res.Changed || res.Mode != modeEnforce {
		t.Errorf("complain reload: got %+v, want Changed=true Mode=enforce", res)
	}
	if !n.ran("apparmor_parser") {
		t.Error("a complain-mode profile should be reloaded")
	}
}

func TestEnsureParserFailure(t *testing.T) {
	n := &fakeNode{parserExit: 1}
	_, err := EnsureAppArmorProfile(context.Background(), Options{Runner: n})
	if err == nil || !strings.Contains(err.Error(), "load AppArmor profile") {
		t.Fatalf("want load-failure error, got %v", err)
	}
}

func TestEnsureAppArmorDisabled(t *testing.T) {
	// The kernel profiles file is unreadable (AppArmor not enabled).
	n := &fakeNode{profilesExit: 1}
	_, err := EnsureAppArmorProfile(context.Background(), Options{Runner: n})
	if err == nil || !strings.Contains(err.Error(), "AppArmor enabled") {
		t.Fatalf("want AppArmor-disabled error, got %v", err)
	}
	// Write-first ordering: the file is installed (harmlessly — /etc/apparmor.d is
	// inert when AppArmor is off) before the load is verified and fails.
	if !n.ran("| base64 -d |") {
		t.Error("expected the profile file to be written before the AppArmor check")
	}
}

func TestEnsurePostReloadQueryTransportError(t *testing.T) {
	// Fresh node: first profiles read succeeds (not loaded), the reload runs, then
	// the post-reload re-verification read fails on the transport.
	n := &fakeNode{otherLoaded: "cri-containerd.apparmor.d", profilesErrOnCall: 2}
	_, err := EnsureAppArmorProfile(context.Background(), Options{Runner: n})
	if err == nil || !strings.Contains(err.Error(), "transport down") {
		t.Fatalf("want wrapped transport error on the post-reload query, got %v", err)
	}
}

func TestEnsureParserSilentFailure(t *testing.T) {
	// apparmor_parser exits 0 but the profile never appears in the kernel list
	// (partially disabled AppArmor): EnsureAppArmorProfile must not call it loaded.
	n := &fakeNode{otherLoaded: "cri-containerd.apparmor.d", parserNoLoad: true}
	_, err := EnsureAppArmorProfile(context.Background(), Options{Runner: n})
	if err == nil || !strings.Contains(err.Error(), "not loaded after apparmor_parser") {
		t.Fatalf("want not-loaded-after-parser error, got %v", err)
	}
}

func TestEnsurePostReloadNotEnforce(t *testing.T) {
	// apparmor_parser loads the profile but in complain mode (e.g. a system-wide
	// complain override): the post-reload verification must reject it.
	n := &fakeNode{otherLoaded: "cri-containerd.apparmor.d", parserMode: "complain"}
	_, err := EnsureAppArmorProfile(context.Background(), Options{Runner: n})
	if err == nil || !strings.Contains(err.Error(), `"complain" mode`) {
		t.Fatalf("want post-reload non-enforce error, got %v", err)
	}
}

func TestEnsureTransportError(t *testing.T) {
	n := &fakeNode{runErrOn: "cat " + profilePath}
	_, err := EnsureAppArmorProfile(context.Background(), Options{Runner: n})
	if err == nil || !strings.Contains(err.Error(), "transport down") {
		t.Fatalf("want wrapped transport error, got %v", err)
	}
}

func TestEnsureParserTransportError(t *testing.T) {
	n := &fakeNode{parserErr: errors.New("session lost")}
	_, err := EnsureAppArmorProfile(context.Background(), Options{Runner: n})
	if err == nil || !strings.Contains(err.Error(), "session lost") {
		t.Fatalf("want wrapped parser transport error, got %v", err)
	}
}

func TestEnsureNilRunner(t *testing.T) {
	if _, err := EnsureAppArmorProfile(context.Background(), Options{}); err == nil {
		t.Fatal("want error for a nil runner")
	}
}

func TestEnsureNonSudoOmitsSudo(t *testing.T) {
	n := &fakeNode{otherLoaded: "cri-containerd.apparmor.d"}
	if _, err := EnsureAppArmorProfile(context.Background(), Options{Runner: n}); err != nil {
		t.Fatalf("EnsureAppArmorProfile: %v", err)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, c := range n.cmds {
		if strings.Contains(c, "sudo ") {
			t.Errorf("non-sudo run issued a sudo command: %q", c)
		}
	}
}

func TestCheckPass(t *testing.T) {
	n := &fakeNode{loaded: true, mode: modeEnforce}
	c := AppArmorProfileLoadedCheck(n, false)
	res, err := c.Probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.Status != check.StatusPass {
		t.Errorf("loaded enforce: got %q, want pass (%s)", res.Status, res.Message)
	}
}

func TestCheckContract(t *testing.T) {
	c := AppArmorProfileLoadedCheck(&fakeNode{}, false)
	if c.ID != IDAppArmorProfileLoaded {
		t.Errorf("check ID = %q, want %q", c.ID, IDAppArmorProfileLoaded)
	}
	// Pin the permanent wire value (it appears in --json output and CI configs).
	if IDAppArmorProfileLoaded != "build.apparmor-profile-loaded" {
		t.Errorf("ID const drifted from its permanent value: %q", IDAppArmorProfileLoaded)
	}
	if c.Severity != check.SeverityCritical {
		t.Errorf("severity = %q, want critical", c.Severity)
	}
	if len(c.Requires) != 0 {
		t.Errorf("check should declare no Requires, got %v", c.Requires)
	}
	if c.Remediation == "" {
		t.Error("check should carry a remediation")
	}
}

func TestCheckFailModes(t *testing.T) {
	// A known-bad-but-determinate state is a StatusFail (the profile is loaded
	// wrong, or definitively absent from a readable list).
	cases := []struct {
		name       string
		node       *fakeNode
		wantSubstr string
	}{
		{"not loaded", &fakeNode{loaded: false, otherLoaded: "cri-containerd.apparmor.d"}, "not loaded"},
		{"complain mode", &fakeNode{loaded: true, mode: "complain"}, "complain"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := AppArmorProfileLoadedCheck(tc.node, true)
			res, err := c.Probe(context.Background())
			if err != nil {
				t.Fatalf("probe returned an error, want a fail Result: %v", err)
			}
			if res.Status != check.StatusFail {
				t.Errorf("status = %q, want fail", res.Status)
			}
			if !strings.Contains(res.Message, tc.wantSubstr) {
				t.Errorf("message %q does not mention %q", res.Message, tc.wantSubstr)
			}
		})
	}
}

func TestCheckIndeterminateMapsToProbeError(t *testing.T) {
	// "couldn't determine" must be a probe ERROR, never a fail or pass — unknown
	// is never hardened. Both a transport failure and an unreadable profiles file
	// (AppArmor disabled, or securityfs not readable — indistinguishable from a
	// non-zero exit) qualify.
	cases := []struct {
		name string
		node *fakeNode
	}{
		{"transport failure", &fakeNode{runErrOn: "cat " + profilesFile}},
		{"profiles unreadable", &fakeNode{profilesExit: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := AppArmorProfileLoadedCheck(tc.node, false)
			res, err := c.Probe(context.Background())
			if err == nil {
				t.Fatalf("want a probe error, got result %+v", res)
			}
		})
	}
}

func TestProfileMode(t *testing.T) {
	const list = "cri-containerd.apparmor.d (enforce)\n" + ProfileName + " (enforce)\nsnap.foo (complain)\n"
	cases := []struct {
		list, name string
		wantMode   string
		wantLoaded bool
	}{
		{list, ProfileName, "enforce", true},
		{ProfileName + " (complain)\n", ProfileName, "complain", true},
		{"other (enforce)\n", ProfileName, "", false},
		{"", ProfileName, "", false},
		{ProfileName + "//child (enforce)\n", ProfileName, "", false}, // a hat, not the profile itself
		{"garbage without parens\n", ProfileName, "", false},
	}
	for _, tc := range cases {
		mode, loaded := profileMode(tc.list, tc.name)
		if mode != tc.wantMode || loaded != tc.wantLoaded {
			t.Errorf("profileMode(%q) = (%q,%v), want (%q,%v)", tc.list, mode, loaded, tc.wantMode, tc.wantLoaded)
		}
	}
}

// TestEmbeddedProfileMatchesConfig guards against the embedded copy drifting from
// the canonical live profile. config/apparmor/orkano-buildkit.profile is the one
// referenced by the build Job securityContext and the substrate smoke; this
// package embeds a copy so the CLI is self-contained, and the two must match.
func TestEmbeddedProfileMatchesConfig(t *testing.T) {
	want, err := os.ReadFile("../../config/apparmor/orkano-buildkit.profile")
	if err != nil {
		t.Fatalf("read canonical profile: %v", err)
	}
	if !bytes.Equal(want, apparmorProfile) {
		t.Error("internal/nodeprep/orkano-buildkit.profile drifted from config/apparmor/orkano-buildkit.profile; copy the latter over it")
	}
}
