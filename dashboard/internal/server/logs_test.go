package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/orkanoio/orkano/dashboard/internal/auth"
)

// --- fake streamer ---

// fakePodStreamer is a deterministic PodLogStreamer for the SSE handler tests. A
// pod's content is a log body (lines separated by \n); block makes each stream
// emit its content then block until its context is cancelled, modelling a live
// `follow` tail.
type fakePodStreamer struct {
	pods      []string
	listErr   error
	content   map[string]string
	streamErr map[string]error
	block     bool

	mu      sync.Mutex
	opts    []PodLogOptions
	ctxs    []context.Context
	listNS  string
	listApp string
}

func (f *fakePodStreamer) ListAppPods(_ context.Context, namespace, appName string) ([]string, error) {
	f.mu.Lock()
	f.listNS, f.listApp = namespace, appName
	f.mu.Unlock()
	return f.pods, f.listErr
}

func (f *fakePodStreamer) lastList() (ns, app string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listNS, f.listApp
}

func (f *fakePodStreamer) StreamPodLog(ctx context.Context, _, pod string, opts PodLogOptions) (io.ReadCloser, error) {
	f.mu.Lock()
	f.opts = append(f.opts, opts)
	f.ctxs = append(f.ctxs, ctx)
	f.mu.Unlock()
	if err := f.streamErr[pod]; err != nil {
		return nil, err
	}
	return &scriptedReader{data: []byte(f.content[pod]), block: f.block, ctx: ctx, closed: make(chan struct{})}, nil
}

func (f *fakePodStreamer) recordedOpts() []PodLogOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]PodLogOptions(nil), f.opts...)
}

func (f *fakePodStreamer) recordedCtxs() []context.Context {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]context.Context(nil), f.ctxs...)
}

// scriptedReader yields data once, then EOFs (block=false) or blocks until its
// context is cancelled or it is closed (block=true) — a `follow` tail.
type scriptedReader struct {
	data   []byte
	off    int
	block  bool
	ctx    context.Context
	closed chan struct{}
	once   sync.Once
}

func (r *scriptedReader) Read(p []byte) (int, error) {
	if r.off < len(r.data) {
		n := copy(p, r.data[r.off:])
		r.off += n
		return n, nil
	}
	if !r.block {
		return 0, io.EOF
	}
	select {
	case <-r.ctx.Done():
		return 0, io.EOF
	case <-r.closed:
		return 0, io.EOF
	}
}

func (r *scriptedReader) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

// --- harness ---

func logsServer(t *testing.T, streamer PodLogStreamer) (*Server, *fakeStore) {
	t.Helper()
	store := newFakeStore()
	k8s := crfake.NewClientBuilder().Build()
	s, err := New(Config{
		K8s:                k8s,
		ViewerClient:       k8s,
		PodLogs:            streamer,
		DB:                 fakePinger{},
		Store:              store,
		Cipher:             testCipherInstance,
		BootstrapTokenHash: auth.HashToken(testBootstrapToken),
		SPA:                testSPA(),
		Now:                fixedNow,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, store
}

// syncRecorder is a minimal ResponseWriter whose buffer is mutex-guarded, so a
// test goroutine can poll the streamed body while the handler goroutine writes
// to it without a data race (httptest.ResponseRecorder's buffer is not safe for
// that). http.ResponseController falls back gracefully: SetWriteDeadline reports
// unsupported (the handler only warns), and Flush is satisfied here.
type syncRecorder struct {
	mu     sync.Mutex
	buf    strings.Builder
	header http.Header
	code   int
}

func newSyncRecorder() *syncRecorder { return &syncRecorder{header: make(http.Header)} }

func (r *syncRecorder) Header() http.Header { return r.header }

func (r *syncRecorder) WriteHeader(code int) {
	r.mu.Lock()
	r.code = code
	r.mu.Unlock()
}

func (r *syncRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(p)
}

func (r *syncRecorder) Flush() {}

func (r *syncRecorder) body() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

// --- SSE parsing ---

type sseEvent struct {
	event string
	data  string
}

// parseSSE splits an SSE body into events. It understands only the `event:` and
// `data:` fields the handler emits (keepalive `:` comments are skipped; `id:`/
// `retry:` are not produced and would be ignored). It relies on the handler's
// contract that each `data:` is a single JSON object with no literal newline.
func parseSSE(t *testing.T, body string) []sseEvent {
	t.Helper()
	var out []sseEvent
	for _, block := range strings.Split(body, "\n\n") {
		block = strings.TrimRight(block, "\n")
		if block == "" {
			continue
		}
		var ev sseEvent
		var hasField bool
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.event, hasField = strings.TrimPrefix(line, "event: "), true
			case strings.HasPrefix(line, "data: "):
				ev.data, hasField = strings.TrimPrefix(line, "data: "), true
				// A line starting with ":" is an SSE keepalive comment; it matches no
				// field prefix, so it is naturally ignored (hasField stays false).
			}
		}
		if hasField {
			out = append(out, ev)
		}
	}
	return out
}

// dataLines returns the decoded {pod,line} of every default-typed data event.
func dataLines(t *testing.T, evs []sseEvent) []map[string]string {
	t.Helper()
	var out []map[string]string
	for _, ev := range evs {
		if ev.event != "" {
			continue
		}
		var m map[string]string
		if err := json.Unmarshal([]byte(ev.data), &m); err != nil {
			t.Fatalf("decode data event %q: %v", ev.data, err)
		}
		out = append(out, m)
	}
	return out
}

func hasEvent(evs []sseEvent, name string) bool {
	for _, ev := range evs {
		if ev.event == name {
			return true
		}
	}
	return false
}

// --- tests ---

func TestAppLogsStreamsAllPods(t *testing.T) {
	streamer := &fakePodStreamer{
		pods:    []string{"web-1", "web-2"},
		content: map[string]string{"web-1": "alpha\nbeta\n", "web-2": "gamma\n"},
	}
	s, store := logsServer(t, streamer)
	ck := authedSession(t, store)

	rec := apiReq(t, s, http.MethodGet, "/api/apps/web/logs?follow=false", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if xb := rec.Header().Get("X-Accel-Buffering"); xb != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no (proxies must not buffer the tail)", xb)
	}
	if ns, app := streamer.lastList(); ns != appsNamespace || app != "web" {
		t.Errorf("listed pods in (ns=%q app=%q), want (orkano-apps, web)", ns, app)
	}

	evs := parseSSE(t, rec.Body.String())
	got := map[string]bool{}
	for _, m := range dataLines(t, evs) {
		got[m["pod"]+"|"+m["line"]] = true
	}
	for _, want := range []string{"web-1|alpha", "web-1|beta", "web-2|gamma"} {
		if !got[want] {
			t.Errorf("missing log line %q; got %v", want, got)
		}
	}
	if !hasEvent(evs, "eof") {
		t.Error("expected a terminating eof event")
	}
}

func TestAppLogsNoPods(t *testing.T) {
	// An app with no running pods (0 replicas, or not yet deployed) is not an
	// error: the stream opens, emits no data, and terminates with eof.
	streamer := &fakePodStreamer{pods: nil}
	s, store := logsServer(t, streamer)
	ck := authedSession(t, store)
	rec := apiReq(t, s, http.MethodGet, "/api/apps/web/logs", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	evs := parseSSE(t, rec.Body.String())
	if len(dataLines(t, evs)) != 0 {
		t.Errorf("expected no data events for a pod-less app; got %+v", evs)
	}
	if !hasEvent(evs, "eof") {
		t.Error("expected a terminating eof event")
	}
}

func TestAppLogsInvalidName(t *testing.T) {
	s, store := logsServer(t, &fakePodStreamer{})
	ck := authedSession(t, store)
	// Uppercase is not a valid DNS-1123 subdomain.
	rec := apiReq(t, s, http.MethodGet, "/api/apps/Bad/logs", nil, ck)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestAppLogsRequiresSession(t *testing.T) {
	s, _ := logsServer(t, &fakePodStreamer{pods: []string{"web-1"}})
	rec := apiReq(t, s, http.MethodGet, "/api/apps/web/logs", nil) // no cookie
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAppLogsSpecificPod(t *testing.T) {
	t.Run("member pod streams only itself", func(t *testing.T) {
		streamer := &fakePodStreamer{
			pods:    []string{"web-1", "web-2"},
			content: map[string]string{"web-1": "only-this\n", "web-2": "not-this\n"},
		}
		s, store := logsServer(t, streamer)
		ck := authedSession(t, store)
		rec := apiReq(t, s, http.MethodGet, "/api/apps/web/logs?follow=false&pod=web-1", nil, ck)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		for _, m := range dataLines(t, parseSSE(t, rec.Body.String())) {
			if m["pod"] != "web-1" {
				t.Errorf("got a line from pod %q, want only web-1", m["pod"])
			}
		}
	})
	t.Run("non-member pod is 404", func(t *testing.T) {
		streamer := &fakePodStreamer{pods: []string{"web-1"}}
		s, store := logsServer(t, streamer)
		ck := authedSession(t, store)
		rec := apiReq(t, s, http.MethodGet, "/api/apps/web/logs?pod=web-9", nil, ck)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		// A rejection before streaming returns JSON, not an SSE body.
		if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
			t.Errorf("rejected request should not be an SSE stream (Content-Type %q)", ct)
		}
	})
	t.Run("any pod against an empty app is 404", func(t *testing.T) {
		// With no pods listed, the allowlist is empty, so even a syntactically valid
		// pod name 404s — the membership gate is fail-closed.
		s, store := logsServer(t, &fakePodStreamer{pods: nil})
		ck := authedSession(t, store)
		rec := apiReq(t, s, http.MethodGet, "/api/apps/web/logs?pod=web-1", nil, ck)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
	t.Run("malformed pod name is 404", func(t *testing.T) {
		s, store := logsServer(t, &fakePodStreamer{pods: []string{"web-1"}})
		ck := authedSession(t, store)
		rec := apiReq(t, s, http.MethodGet, "/api/apps/web/logs?pod=Bad_Pod", nil, ck)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
}

func TestAppLogsMixedPodOutcomes(t *testing.T) {
	// One pod streams lines while another fails to open: the handler must surface
	// the healthy pod's data AND an error event for the failed pod, then eof.
	streamer := &fakePodStreamer{
		pods:      []string{"web-1", "web-2"},
		content:   map[string]string{"web-1": "healthy\n"},
		streamErr: map[string]error{"web-2": errors.New("container creating")},
	}
	s, store := logsServer(t, streamer)
	ck := authedSession(t, store)
	rec := apiReq(t, s, http.MethodGet, "/api/apps/web/logs?follow=false", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	evs := parseSSE(t, rec.Body.String())
	var sawHealthy bool
	for _, m := range dataLines(t, evs) {
		if m["pod"] == "web-1" && m["line"] == "healthy" {
			sawHealthy = true
		}
	}
	if !sawHealthy {
		t.Error("healthy pod's line missing despite a sibling pod's stream error")
	}
	if !hasEvent(evs, "error") {
		t.Error("expected an error event for the failed pod")
	}
	if !hasEvent(evs, "eof") {
		t.Error("expected a terminating eof event")
	}
}

func TestAppLogsListError(t *testing.T) {
	streamer := &fakePodStreamer{listErr: errors.New("apiserver boom")}
	s, store := logsServer(t, streamer)
	ck := authedSession(t, store)
	rec := apiReq(t, s, http.MethodGet, "/api/apps/web/logs", nil, ck)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestAppLogsStreamErrorEvent(t *testing.T) {
	streamer := &fakePodStreamer{
		pods:      []string{"web-1"},
		streamErr: map[string]error{"web-1": errors.New("container not ready")},
	}
	s, store := logsServer(t, streamer)
	ck := authedSession(t, store)
	rec := apiReq(t, s, http.MethodGet, "/api/apps/web/logs?follow=false", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a per-pod error is an in-stream event, not a status)", rec.Code)
	}
	evs := parseSSE(t, rec.Body.String())
	if !hasEvent(evs, "error") {
		t.Fatalf("expected an error event; got %+v", evs)
	}
	for _, ev := range evs {
		if ev.event == "error" && strings.Contains(ev.data, "container not ready") {
			t.Error("error event leaked the raw cluster error to the client")
		}
	}
}

func TestAppLogsOptionParsing(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		streamer := &fakePodStreamer{pods: []string{"web-1"}, content: map[string]string{"web-1": "x\n"}}
		s, store := logsServer(t, streamer)
		ck := authedSession(t, store)
		apiReq(t, s, http.MethodGet, "/api/apps/web/logs?follow=false", nil, ck)
		opts := streamer.recordedOpts()
		if len(opts) != 1 {
			t.Fatalf("recorded %d streams, want 1", len(opts))
		}
		if opts[0].Container != appContainerName {
			t.Errorf("default container = %q, want %q", opts[0].Container, appContainerName)
		}
		if opts[0].TailLines != defaultTailLines {
			t.Errorf("default tail = %d, want %d", opts[0].TailLines, defaultTailLines)
		}
	})
	t.Run("tail is capped", func(t *testing.T) {
		streamer := &fakePodStreamer{pods: []string{"web-1"}, content: map[string]string{"web-1": "x\n"}}
		s, store := logsServer(t, streamer)
		ck := authedSession(t, store)
		apiReq(t, s, http.MethodGet, "/api/apps/web/logs?follow=false&tail=999999", nil, ck)
		if got := streamer.recordedOpts()[0].TailLines; got != maxTailLines {
			t.Errorf("tail = %d, want capped to %d", got, maxTailLines)
		}
	})
	t.Run("tail=0 is honoured as zero history", func(t *testing.T) {
		// 0 is valid and distinct from the default (200) and the cap: it means
		// "replay no history". The clientset impl sets TailLines whenever it is >= 0.
		streamer := &fakePodStreamer{pods: []string{"web-1"}, content: map[string]string{"web-1": "x\n"}}
		s, store := logsServer(t, streamer)
		ck := authedSession(t, store)
		apiReq(t, s, http.MethodGet, "/api/apps/web/logs?follow=false&tail=0", nil, ck)
		if got := streamer.recordedOpts()[0].TailLines; got != 0 {
			t.Errorf("tail = %d, want 0", got)
		}
	})
	t.Run("previous implies no-follow", func(t *testing.T) {
		// follow defaults to true; previous (a one-shot read of a terminated
		// container) must override it, since follow+previous is invalid.
		streamer := &fakePodStreamer{pods: []string{"web-1"}, content: map[string]string{"web-1": "x\n"}}
		s, store := logsServer(t, streamer)
		ck := authedSession(t, store)
		apiReq(t, s, http.MethodGet, "/api/apps/web/logs?container=sidecar&previous=true", nil, ck)
		got := streamer.recordedOpts()[0]
		if got.Container != "sidecar" || !got.Previous || got.Follow {
			t.Errorf("opts = %+v, want container=sidecar previous=true follow=false", got)
		}
	})
	t.Run("malformed values are 400", func(t *testing.T) {
		for _, q := range []string{"follow=maybe", "previous=2", "tail=-1", "tail=abc", "container=Bad_Name"} {
			s, store := logsServer(t, &fakePodStreamer{pods: []string{"web-1"}})
			ck := authedSession(t, store)
			rec := apiReq(t, s, http.MethodGet, "/api/apps/web/logs?"+q, nil, ck)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("query %q: status = %d, want 400", q, rec.Code)
			}
		}
	})
}

func TestAppLogsClientDisconnectTearsDownReaders(t *testing.T) {
	streamer := &fakePodStreamer{pods: []string{"web-1"}, content: map[string]string{"web-1": "live\n"}, block: true}
	s, store := logsServer(t, streamer)
	ck := authedSession(t, store)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/apps/web/logs", nil)
	req.AddCookie(ck)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() { s.Handler().ServeHTTP(rec, req); close(done) }()

	// Wait until the handler has opened the pod stream (recorded its context).
	waitFor(t, 2*time.Second, func() bool { return len(streamer.recordedCtxs()) == 1 })

	cancel() // client disconnects
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after client disconnect")
	}

	// The per-pod reader's context must have been cancelled (stream torn down).
	streamCtx := streamer.recordedCtxs()[0]
	select {
	case <-streamCtx.Done():
	default:
		t.Error("stream context was not cancelled on client disconnect")
	}
}

// TestAppLogsHeartbeat mutates the package var logHeartbeat, so it (like the
// other streaming tests) must NOT be made t.Parallel().
func TestAppLogsHeartbeat(t *testing.T) {
	old := logHeartbeat
	logHeartbeat = 5 * time.Millisecond
	defer func() { logHeartbeat = old }()

	// A pod that produces no output and blocks, so only heartbeats are written.
	streamer := &fakePodStreamer{pods: []string{"web-1"}, block: true}
	s, store := logsServer(t, streamer)
	ck := authedSession(t, store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/apps/web/logs", nil)
	req.AddCookie(ck)
	rec := newSyncRecorder()

	done := make(chan struct{})
	go func() { s.Handler().ServeHTTP(rec, req); close(done) }()

	// Poll the synchronized body until a keepalive comment appears, rather than
	// sleeping a fixed duration (which a loaded runner could starve).
	waitFor(t, 2*time.Second, func() bool { return strings.Contains(rec.body(), ":\n\n") })
	cancel()
	<-done
}

func TestClientsetPodLogStreamer(t *testing.T) {
	mkPod := func(name, app string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: appsNamespace,
			Labels:    map[string]string{appPodLabel: app},
		}}
	}
	cs := k8sfake.NewSimpleClientset(
		mkPod("web-1", "web"),
		mkPod("web-2", "web"),
		mkPod("api-1", "api"),
	)
	st := NewPodLogStreamer(cs)

	names, err := st.ListAppPods(context.Background(), appsNamespace, "web")
	if err != nil {
		t.Fatalf("ListAppPods: %v", err)
	}
	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}
	if !got["web-1"] || !got["web-2"] || got["api-1"] {
		t.Errorf("ListAppPods returned %v, want only web-1+web-2 (label-filtered)", names)
	}

	rc, err := st.StreamPodLog(context.Background(), appsNamespace, "web-1", PodLogOptions{Container: appContainerName, TailLines: 10})
	if err != nil {
		t.Fatalf("StreamPodLog: %v", err)
	}
	defer func() { _ = rc.Close() }()
	if _, err := io.ReadAll(rc); err != nil {
		t.Fatalf("read stream: %v", err)
	}

	// The fake records the PodLogOptions it was called with, so assert the impl
	// forwards container + tail correctly (the canned body above can't prove it).
	var forwarded bool
	for _, a := range cs.Actions() {
		if a.GetSubresource() != "log" || a.GetNamespace() != appsNamespace {
			continue
		}
		ga, ok := a.(clienttesting.GenericAction)
		if !ok {
			continue
		}
		plo, ok := ga.GetValue().(*corev1.PodLogOptions)
		if ok && plo.Container == appContainerName && plo.TailLines != nil && *plo.TailLines == 10 {
			forwarded = true
		}
	}
	if !forwarded {
		t.Error("StreamPodLog did not forward container/tail into the apiserver GetLogs call")
	}
}

// waitFor polls cond until it is true or the deadline elapses.
func waitFor(t *testing.T, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}
