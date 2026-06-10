// Spike 3: GitHub App manifest flow.
// Serves a one-page auto-submitting form that hands an app manifest to
// GitHub, then exchanges the returned temporary code for the app's
// credentials. Operator steps live in runbook.md.
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

const (
	projectURL      = "https://github.com/orkanoio/orkano"
	outDir          = "out"
	pemFileName     = "app.private-key.pem"
	appJSONFileName = "app.json"
)

const indexPage = `<!doctype html>
<html>
<head><meta charset="utf-8"><title>Orkano spike: create GitHub App</title></head>
<body>
<p>Submitting the manifest for <strong>{{.AppName}}</strong> to GitHub&hellip;</p>
<form method="post" action="{{.Action}}">
<input type="hidden" name="manifest" value="{{.Manifest}}">
<noscript><button type="submit">Create GitHub App</button></noscript>
</form>
<script>document.forms[0].submit();</script>
</body>
</html>
`

const successPage = `<!doctype html>
<html>
<head><meta charset="utf-8"><title>Orkano spike: done</title></head>
<body>
<p>GitHub App <strong>{{.Slug}}</strong> created. Credentials written to <code>out/</code>.</p>
<p>You can close this tab; the helper has shut down. The terminal has the summary.</p>
</body>
</html>
`

type config struct {
	org        string
	port       int
	appName    string
	webhookURL string
}

type manifest struct {
	Name               string            `json:"name"`
	URL                string            `json:"url"`
	HookAttributes     hookAttributes    `json:"hook_attributes"`
	RedirectURL        string            `json:"redirect_url"`
	CallbackURLs       []string          `json:"callback_urls"`
	Public             bool              `json:"public"`
	DefaultPermissions map[string]string `json:"default_permissions"`
	DefaultEvents      []string          `json:"default_events"`
}

type hookAttributes struct {
	URL string `json:"url"`
}

type conversion struct {
	ID            int64  `json:"id"`
	Slug          string `json:"slug"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	WebhookSecret string `json:"webhook_secret"`
	PEM           string `json:"pem"`
	HTMLURL       string `json:"html_url"`
	Owner         owner  `json:"owner"`
}

type owner struct {
	Login string `json:"login"`
}

type server struct {
	cfg         config
	state       string
	indexTmpl   *template.Template
	successTmpl *template.Template
	client      *http.Client
	done        chan struct{}
	finishOnce  sync.Once
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	var cfg config
	flag.StringVar(&cfg.org, "org", "", "GitHub organization to create the app under (empty = personal account)")
	flag.IntVar(&cfg.port, "port", 8765, "local port for the form and callback")
	flag.StringVar(&cfg.appName, "app-name", "orkano-dev-test", "name of the GitHub App to create")
	flag.StringVar(&cfg.webhookURL, "webhook-url", "https://example.invalid/webhook", "webhook URL baked into the manifest (placeholder is fine for the spike)")
	flag.Parse()
	if cfg.port < 1 || cfg.port > 65535 {
		return fmt.Errorf("invalid --port %d: must be between 1 and 65535", cfg.port)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	state, err := randomState()
	if err != nil {
		return fmt.Errorf("generate state token: %w", err)
	}

	s := &server{
		cfg:         cfg,
		state:       state,
		indexTmpl:   template.Must(template.New("index").Parse(indexPage)),
		successTmpl: template.Must(template.New("success").Parse(successPage)),
		client:      &http.Client{Timeout: 30 * time.Second},
		done:        make(chan struct{}),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /callback", s.handleCallback)

	addr := fmt.Sprintf("127.0.0.1:%d", cfg.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w — the port is likely taken; rerun with --port <free-port>", addr, err)
	}

	httpSrv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	serveErr := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	startURL := fmt.Sprintf("http://localhost:%d/", cfg.port)
	target := "your personal account"
	if cfg.org != "" {
		target = "org " + cfg.org
	}
	fmt.Printf("Creating GitHub App %q under %s.\nOpen %s if the browser does not open by itself.\n", cfg.appName, target, startURL)
	if err := openBrowser(ctx, startURL); err != nil {
		fmt.Printf("(could not open a browser automatically: %v)\n", err)
	}

	finished := false
	select {
	case err := <-serveErr:
		return fmt.Errorf("serve on %s: %w", addr, err)
	case <-ctx.Done():
	case <-s.done:
		finished = true
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shut down http server: %w", err)
	}
	if !finished {
		fmt.Println("interrupted before the app was created — no credentials were written")
	}
	return nil
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	m := manifest{
		Name:               s.cfg.appName,
		URL:                projectURL,
		HookAttributes:     hookAttributes{URL: s.cfg.webhookURL},
		RedirectURL:        s.callbackURL(),
		CallbackURLs:       []string{s.callbackURL()},
		Public:             false,
		DefaultPermissions: map[string]string{"contents": "read", "metadata": "read"},
		DefaultEvents:      []string{"push"},
	}
	body, err := json.Marshal(m)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "marshal manifest: %v", err)
		return
	}
	data := struct {
		AppName  string
		Action   string
		Manifest string
	}{s.cfg.appName, s.newAppURL(), string(body)}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.indexTmpl.Execute(w, data); err != nil {
		fmt.Fprintf(os.Stderr, "render index page: %v\n", err)
	}
}

func (s *server) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	code := q.Get("code")
	if code == "" {
		s.fail(w, http.StatusBadRequest, "callback is missing the code parameter — GitHub did not finish creating the app; restart from http://localhost:%d/", s.cfg.port)
		return
	}
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(s.state)) != 1 {
		s.fail(w, http.StatusBadRequest, "state mismatch on callback — this redirect did not come from the form this run served; close stale GitHub tabs and restart from http://localhost:%d/", s.cfg.port)
		return
	}

	conv, redacted, err := s.exchangeCode(r.Context(), code)
	if err != nil {
		s.fail(w, http.StatusBadGateway, "exchange manifest code: %v", err)
		return
	}
	if err := writeCredentials(conv, redacted); err != nil {
		s.fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	printSummary(conv)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.successTmpl.Execute(w, conv); err != nil {
		fmt.Fprintf(os.Stderr, "render success page: %v\n", err)
	}
	s.finishOnce.Do(func() { close(s.done) })
}

// The temporary code GitHub appends to the redirect is single use and expires
// after about one hour, so it is exchanged immediately, before anything else.
func (s *server) exchangeCode(ctx context.Context, code string) (conversion, []byte, error) {
	u := "https://api.github.com/app-manifests/" + url.PathEscape(code) + "/conversions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return conversion{}, nil, fmt.Errorf("build conversion request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := s.client.Do(req)
	if err != nil {
		return conversion{}, nil, fmt.Errorf("POST https://api.github.com/app-manifests/{code}/conversions: %w — check connectivity to api.github.com, then restart from http://localhost:%d/", err, s.cfg.port)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return conversion{}, nil, fmt.Errorf("read conversion response: %w", err)
	}
	if resp.StatusCode != http.StatusCreated {
		return conversion{}, nil, fmt.Errorf("GitHub answered %s to the conversion (body: %s) — the code is single use and expires after about an hour; restart from http://localhost:%d/ to mint a fresh one", resp.Status, truncate(body, 300), s.cfg.port)
	}

	var conv conversion
	if err := json.Unmarshal(body, &conv); err != nil {
		return conversion{}, nil, fmt.Errorf("decode conversion response: %w", err)
	}
	if conv.PEM == "" {
		return conversion{}, nil, errors.New("conversion response has an empty pem field — nothing was written; inspect the response body and retry the flow")
	}

	var full map[string]any
	if err := json.Unmarshal(body, &full); err != nil {
		return conversion{}, nil, fmt.Errorf("decode conversion response for redaction: %w", err)
	}
	delete(full, "pem")
	redacted, err := json.MarshalIndent(full, "", "  ")
	if err != nil {
		return conversion{}, nil, fmt.Errorf("re-encode redacted app metadata: %w", err)
	}
	return conv, redacted, nil
}

func writeCredentials(conv conversion, redacted []byte) error {
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return fmt.Errorf("create %s/: %w — check permissions on the spike directory", outDir, err)
	}
	pemPath := filepath.Join(outDir, pemFileName)
	if err := os.WriteFile(pemPath, []byte(conv.PEM), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", pemPath, err)
	}
	appPath := filepath.Join(outDir, appJSONFileName)
	if err := os.WriteFile(appPath, append(redacted, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", appPath, err)
	}
	return nil
}

func printSummary(conv conversion) {
	fmt.Printf(`
GitHub App created.

  id:        %d
  slug:      %s
  owner:     %s
  client_id: %s
  app page:  %s

Written:
  %s   app metadata, client_secret, webhook_secret (pem excluded)
  %s   RSA private key

Install it when ready: https://github.com/apps/%s/installations/new
`,
		conv.ID, conv.Slug, conv.Owner.Login, conv.ClientID, conv.HTMLURL,
		filepath.Join(outDir, appJSONFileName), filepath.Join(outDir, pemFileName), conv.Slug)
}

func (s *server) fail(w http.ResponseWriter, status int, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintln(os.Stderr, "error:", msg)
	http.Error(w, msg, status)
}

func (s *server) callbackURL() string {
	return fmt.Sprintf("http://localhost:%d/callback", s.cfg.port)
}

func (s *server) newAppURL() string {
	base := "https://github.com/settings/apps/new"
	if s.cfg.org != "" {
		base = "https://github.com/organizations/" + url.PathEscape(s.cfg.org) + "/settings/apps/new"
	}
	return base + "?state=" + url.QueryEscape(s.state)
}

func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

func openBrowser(ctx context.Context, u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", u)
	case "linux":
		cmd = exec.CommandContext(ctx, "xdg-open", u)
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", u)
	default:
		return fmt.Errorf("no known browser opener for %s", runtime.GOOS)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", cmd.Path, err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
