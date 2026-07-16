package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
)

// These labels and container names MUST match the operator's immutable pod
// selectors. Renaming either side breaks the corresponding resource stream.
const (
	appPodLabel           = "app.orkano.io/app"
	appContainerName      = "app"
	postgresPodLabel      = "app.orkano.io/postgres"
	postgresContainerName = "postgres"
	mongoPodLabel         = "app.orkano.io/mongo"
	mongoContainerName    = "mongo"
)

const (
	defaultTailLines = 200
	maxTailLines     = 5000
	// logEventBuffer is the depth of the channel merging per-pod readers into the
	// single SSE writer; a slow client backpressures the readers, never the server.
	logEventBuffer = 64
)

// logHeartbeat is the interval between SSE keepalive comments. It keeps an idle
// connection from being closed by an intermediary and lets the writer notice a
// vanished client between log lines. A var so tests can shrink it.
var logHeartbeat = 20 * time.Second

// PodLogOptions parameterizes one pod's log stream.
type PodLogOptions struct {
	Container string
	Follow    bool
	Previous  bool
	// TailLines bounds how many existing lines to replay before streaming new
	// output. >= 0 sets the limit (0 = no history, only new lines under Follow);
	// < 0 leaves it unset so the server returns the full log. The handler always
	// sets a value in [0, maxTailLines]; a bare PodLogOptions{} replays 0 lines.
	TailLines int
}

// PodLogStreamer lists a resource's pods and streams their container logs under the
// dashboard's impersonated viewer identity (ADR-0015) — so the cluster's RBAC +
// audit trail attribute the read to the fixed view-only identity, never the
// dashboard ServiceAccount. The production implementation wraps a client-go
// clientset built from the viewer rest.Config (NewViewerPodLogStreamer); tests
// supply a fake.
type PodLogStreamer interface {
	// ListPods returns pod names matching one exact label in namespace.
	ListPods(ctx context.Context, namespace, labelKey, labelValue string) ([]string, error)
	// StreamPodLog opens a log stream for one pod. With Follow set it stays open
	// until the context is cancelled or the pod terminates.
	StreamPodLog(ctx context.Context, namespace, pod string, opts PodLogOptions) (io.ReadCloser, error)
}

// clientsetPodLogStreamer is the production PodLogStreamer: it lists pods and
// streams logs via a client-go clientset. The dashboard builds the clientset
// from the viewer-impersonating rest.Config, so every call carries the viewer
// identity.
type clientsetPodLogStreamer struct {
	cs kubernetes.Interface
}

// NewPodLogStreamer wraps a clientset as a PodLogStreamer. The clientset must
// already carry the desired identity — the dashboard builds it from the
// viewer-impersonating config via NewViewerPodLogStreamer.
func NewPodLogStreamer(cs kubernetes.Interface) PodLogStreamer {
	return &clientsetPodLogStreamer{cs: cs}
}

func (c *clientsetPodLogStreamer) ListPods(ctx context.Context, namespace, labelKey, labelValue string) ([]string, error) {
	sel := labels.SelectorFromSet(labels.Set{labelKey: labelValue}).String()
	list, err := c.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		names = append(names, list.Items[i].Name)
	}
	return names, nil
}

func (c *clientsetPodLogStreamer) StreamPodLog(ctx context.Context, namespace, pod string, opts PodLogOptions) (io.ReadCloser, error) {
	plo := &corev1.PodLogOptions{
		Container: opts.Container,
		Follow:    opts.Follow,
		Previous:  opts.Previous,
	}
	if opts.TailLines >= 0 {
		tl := int64(opts.TailLines)
		plo.TailLines = &tl
	}
	return c.cs.CoreV1().Pods(namespace).GetLogs(pod, plo).Stream(ctx)
}

// logEvent is one item flowing from a per-pod reader to the SSE writer: a log
// line, or a stream error tagged with the pod it came from.
type logEvent struct {
	pod  string
	line string
	err  error
}

// handleAppLogs streams an App's pod logs to the client as Server-Sent Events.
// It is a read view (RequireSession) routed through the impersonating streamer.
// SSE rather than WebSocket because log tailing is strictly server→client: the
// framing is a few writes and it adds no dependency. Per-pod readers run as
// goroutines and merge into this handler goroutine, which is the sole writer to
// the ResponseWriter (never written concurrently).
func (s *Server) handleAppLogs(w http.ResponseWriter, r *http.Request) {
	s.handleResourceLogs(w, r, appPodLabel, appContainerName, "apps.logs.list_pods")
}

func (s *Server) handlePostgresLogs(w http.ResponseWriter, r *http.Request) {
	s.handleResourceLogs(w, r, postgresPodLabel, postgresContainerName, "postgres.logs.list_pods")
}

func (s *Server) handleMongoLogs(w http.ResponseWriter, r *http.Request) {
	s.handleResourceLogs(w, r, mongoPodLabel, mongoContainerName, "mongo.logs.list_pods")
}

func (s *Server) handleResourceLogs(w http.ResponseWriter, r *http.Request, podLabel, container, action string) {
	name := chi.URLParam(r, "name")
	if !validResourceName(name) {
		writeJSONError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	s.handleLabeledLogs(w, r, appsNamespace, podLabel, name, container, action)
}

func (s *Server) handleLabeledLogs(w http.ResponseWriter, r *http.Request, namespace, podLabel, labelValue, container, action string) {
	opts, ok := parseLogOptions(w, r, container)
	if !ok {
		return
	}

	ctx := r.Context()
	pods, err := s.cfg.PodLogs.ListPods(ctx, namespace, podLabel, labelValue)
	if err != nil {
		s.writeK8sError(w, action, err)
		return
	}

	// A specific ?pod= must belong to the app: a session can only read logs of
	// pods the impersonated viewer would already see for this app, never an
	// arbitrary pod name elsewhere in the namespace. The DNS-1123 pre-check is
	// defense in depth — a syntactically impossible name can never be a real pod —
	// while the membership test is the actual authorization gate.
	if wantPod := r.URL.Query().Get("pod"); wantPod != "" {
		if len(validation.IsDNS1123Subdomain(wantPod)) != 0 || !slices.Contains(pods, wantPod) {
			writeJSONError(w, http.StatusNotFound, "not_found")
			return
		}
		pods = []string{wantPod}
	}

	// Reads are not audited (INV-08 covers privileged mutations; the other read
	// views — list apps, deploys, audit — are likewise unaudited, and the apiserver
	// audit log records this pods/log access under the impersonated viewer). Revisit
	// per-read auditing if multi-user OIDC makes the logs view a covert channel.
	s.streamLogs(w, r, namespace, pods, opts)
}

// parseLogOptions reads the query params into PodLogOptions, writing a 400 and
// returning false on any malformed value.
func parseLogOptions(w http.ResponseWriter, r *http.Request, defaultContainer string) (PodLogOptions, bool) {
	q := r.URL.Query()
	opts := PodLogOptions{Container: defaultContainer, Follow: true, TailLines: defaultTailLines}

	if c := q.Get("container"); c != "" {
		// Container names are DNS-1123 labels; reject anything else rather than
		// forwarding it to the apiserver.
		if len(validation.IsDNS1123Label(c)) != 0 {
			writeJSONError(w, http.StatusBadRequest, "invalid_request")
			return PodLogOptions{}, false
		}
		opts.Container = c
	}
	for _, b := range []struct {
		key string
		dst *bool
	}{{"follow", &opts.Follow}, {"previous", &opts.Previous}} {
		if v := q.Get(b.key); v != "" {
			parsed, err := strconv.ParseBool(v)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "invalid_request")
				return PodLogOptions{}, false
			}
			*b.dst = parsed
		}
	}
	if v := q.Get("tail"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeJSONError(w, http.StatusBadRequest, "invalid_request")
			return PodLogOptions{}, false
		}
		if n > maxTailLines {
			n = maxTailLines
		}
		opts.TailLines = n
	}
	// A terminated container's previous logs cannot be followed (the apiserver
	// rejects follow+previous); previous is inherently a one-shot read.
	if opts.Previous {
		opts.Follow = false
	}
	return opts, true
}

// streamLogs commits to a 200 text/event-stream response and pumps merged pod
// logs to the client until every pod's stream ends (follow=false or all pods
// terminate) or the client disconnects.
func (s *Server) streamLogs(w http.ResponseWriter, r *http.Request, namespace string, pods []string, opts PodLogOptions) {
	rc := http.NewResponseController(w)
	// The dashboard's http.Server sets a WriteTimeout that would sever a live tail;
	// clear the per-request write deadline. A ResponseWriter that cannot (e.g. a
	// test recorder) is harmless — the warn just records it.
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		s.log.Warn("logs: cannot clear write deadline", "err", err)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // ask intermediaries not to buffer the stream
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()

	ctx := r.Context()
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel() // tear down all pod readers when this handler returns

	events := make(chan logEvent, logEventBuffer)
	// send blocks on streamCtx (the readers' teardown signal), NOT the request ctx:
	// a slow client backpressures a reader here until the channel drains or the
	// handler tears the stream down — it must never wedge a reader past teardown.
	send := func(ev logEvent) bool {
		select {
		case events <- ev:
			return true
		case <-streamCtx.Done():
			return false
		}
	}

	var wg sync.WaitGroup
	for _, pod := range pods {
		wg.Add(1)
		go func(pod string) {
			defer wg.Done()
			rcl, err := s.cfg.PodLogs.StreamPodLog(streamCtx, namespace, pod, opts)
			if err != nil {
				send(logEvent{pod: pod, err: err})
				return
			}
			defer func() { _ = rcl.Close() }()
			br := bufio.NewReader(rcl)
			for {
				line, readErr := br.ReadString('\n')
				if len(line) > 0 {
					if !send(logEvent{pod: pod, line: strings.TrimRight(line, "\r\n")}) {
						return
					}
				}
				if readErr != nil {
					if readErr != io.EOF && streamCtx.Err() == nil {
						send(logEvent{pod: pod, err: readErr})
					}
					return
				}
			}
		}(pod)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	heartbeat := time.NewTicker(logHeartbeat)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return // client disconnected; deferred cancel tears down the readers
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ":\n\n"); err != nil { // SSE comment = keepalive
				return
			}
			if err := rc.Flush(); err != nil {
				return // client gone
			}
		case ev := <-events:
			if !s.writeLogEvent(w, rc, ev) {
				return
			}
		case <-done:
			// All readers finished. Drain anything still buffered, then signal EOF.
			for {
				select {
				case ev := <-events:
					if !s.writeLogEvent(w, rc, ev) {
						return
					}
				default:
					s.writeSSE(w, rc, "eof", `{"reason":"streams ended"}`)
					return
				}
			}
		}
	}
}

// writeLogEvent renders one logEvent as an SSE message. A stream error becomes
// an `error` event carrying a stable code (the raw cluster error stays in the
// server log, never the client), a line becomes a default-typed `data` event.
// Returns false if the write failed (client gone).
func (s *Server) writeLogEvent(w http.ResponseWriter, rc *http.ResponseController, ev logEvent) bool {
	if ev.err != nil {
		s.log.Warn("logs: pod stream error", "pod", ev.pod, "err", ev.err)
		payload, _ := json.Marshal(map[string]string{"pod": ev.pod, "error": "stream_error"})
		return s.writeSSE(w, rc, "error", string(payload))
	}
	payload, _ := json.Marshal(map[string]string{"pod": ev.pod, "line": ev.line})
	return s.writeSSE(w, rc, "", string(payload))
}

// writeSSE writes one Server-Sent Event and flushes it. data is a single JSON
// object (no embedded newlines, since json.Marshal escapes them), so the
// single-line `data:` framing is always valid. event must be a hardcoded literal
// with no newlines (all call sites pass "", "error", or "eof") — a user-derived
// value would allow SSE frame injection. Returns false on a write/flush failure
// (client gone), so the writer loop stops promptly.
func (s *Server) writeSSE(w http.ResponseWriter, rc *http.ResponseController, event, data string) bool {
	var b strings.Builder
	if event != "" {
		b.WriteString("event: ")
		b.WriteString(event)
		b.WriteByte('\n')
	}
	b.WriteString("data: ")
	b.WriteString(data)
	b.WriteString("\n\n")
	if _, err := io.WriteString(w, b.String()); err != nil {
		return false
	}
	return rc.Flush() == nil
}
