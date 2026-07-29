package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
	"github.com/orkanoio/orkano/internal/repoallowlist"
)

func buildFixture(name, appName, phase string, created time.Time) *orkanov1alpha1.Build {
	return &orkanov1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         appsNamespace,
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: orkanov1alpha1.BuildSpec{
			AppName: appName,
			Commit:  "abcdef0123456789abcdef0123456789abcdef01",
			Source:  orkanov1alpha1.Source{GitHub: &orkanov1alpha1.GitHubSource{Repo: "orkanoio/web"}},
			Strategy: orkanov1alpha1.BuildStrategy{
				Strategy: orkanov1alpha1.StrategyDockerfile,
			},
		},
		Status: orkanov1alpha1.BuildStatus{Phase: orkanov1alpha1.BuildPhase(phase)},
	}
}

func TestListBuildsReturnsRealAttemptsNewestFirst(t *testing.T) {
	app := &orkanov1alpha1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: appsNamespace},
		Spec:       webAppSpec(),
	}
	older := buildFixture("web-old", "web", "Failed", fixedNow().Add(-time.Hour))
	newer := buildFixture("web-new", "web", "Running", fixedNow())
	other := buildFixture("other-new", "other", "Succeeded", fixedNow().Add(time.Minute))
	store := newFakeStore()
	store.setSetting(settingGitHubConnectedAt, fixedNow().Format(time.RFC3339))
	s := apiServer(t, store, app, older, newer, other, repoAllowlistFixture(t, "ORKANOIO/DEMO"))
	s.cfg.WebhookURL = "https://hooks.example.com/webhook"
	ck := authedSession(t, store)

	rec := apiReq(t, s, http.MethodGet, "/api/apps/web/builds", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("list builds = %d (%s)", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body["automaticDeploys"] != true {
		t.Fatalf("automaticDeploys = %v, want true for case-insensitive allowlist match", body["automaticDeploys"])
	}
	if body["githubConfigured"] != true || body["githubConnected"] != true {
		t.Fatalf("GitHub setup state = configured:%v connected:%v, want both true",
			body["githubConfigured"], body["githubConnected"])
	}
	items, _ := body["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %d, want only web's two Builds", len(items))
	}
	first, _ := items[0].(map[string]any)
	if first["name"] != "web-new" {
		t.Fatalf("first Build = %v, want newest web-new", first["name"])
	}
}

func TestListBuildsReadsAllowlistAndGitHubStateLive(t *testing.T) {
	app := &orkanov1alpha1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: appsNamespace},
		Spec:       webAppSpec(),
	}
	store := newFakeStore()
	configMap := repoAllowlistFixture(t)
	s := apiServer(t, store, app, configMap)
	ck := authedSession(t, store)

	rec := apiReq(t, s, http.MethodGet, "/api/apps/web/builds", nil, ck)
	body := decodeBody(t, rec)
	if body["automaticDeploys"] != false || body["githubConfigured"] != false ||
		body["githubConnected"] != false {
		t.Fatalf("fresh GitHub state = %+v, want automatic/configured/connected false", body)
	}

	var live corev1.ConfigMap
	key := client.ObjectKey{Namespace: repoallowlist.Namespace, Name: repoallowlist.ConfigMapName}
	if err := s.cfg.K8s.Get(context.Background(), key, &live); err != nil {
		t.Fatalf("get allowlist: %v", err)
	}
	live.Data[repoallowlist.DataKey] = "orkanoio/demo\n"
	if err := s.cfg.K8s.Update(context.Background(), &live); err != nil {
		t.Fatalf("update allowlist: %v", err)
	}
	store.setSetting(settingGitHubConnectedAt, fixedNow().Format(time.RFC3339))
	s.cfg.WebhookURL = "https://hooks.example.com/webhook"

	rec = apiReq(t, s, http.MethodGet, "/api/apps/web/builds", nil, ck)
	body = decodeBody(t, rec)
	if body["automaticDeploys"] != true || body["githubConfigured"] != true ||
		body["githubConnected"] != true {
		t.Fatalf("updated GitHub state = %+v, want automatic/configured/connected true", body)
	}
}

func TestListBuildsNonGitHubSourcesIgnoreGitHubState(t *testing.T) {
	for _, tc := range []struct {
		name        string
		source      orkanov1alpha1.Source
		displayName string
		configMap   *corev1.ConfigMap
	}{
		{
			name: "generic Git with missing allowlist",
			source: orkanov1alpha1.Source{Git: &orkanov1alpha1.GitSource{
				URL: "https://git.example.com/acme/widgets.git",
			}},
			displayName: "https://git.example.com/acme/widgets.git",
		},
		{
			name: "ZIP with malformed allowlist",
			source: orkanov1alpha1.Source{Upload: &orkanov1alpha1.UploadSource{
				Digest:   "sha256:" + strings.Repeat("0", 64),
				FileName: "widgets.zip",
			}},
			displayName: "widgets.zip",
			configMap: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: repoallowlist.Namespace,
					Name:      repoallowlist.ConfigMapName,
				},
				Data: map[string]string{repoallowlist.DataKey: "not-a-repository\n"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := &orkanov1alpha1.App{
				ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: appsNamespace},
				Spec: orkanov1alpha1.AppSpec{
					Source: tc.source,
				},
			}
			objects := []client.Object{app}
			if tc.configMap != nil {
				objects = append(objects, tc.configMap)
			}
			k8s := fake.NewClientBuilder().
				WithScheme(testScheme(t)).
				WithObjects(objects...).
				Build()
			store := newFakeStore()
			store.settingsErr = errors.New("settings unavailable")
			s := serverWith(t, store, k8s)
			ck := authedSession(t, store)

			rec := apiReq(t, s, http.MethodGet, "/api/apps/web/builds", nil, ck)
			if rec.Code != http.StatusOK {
				t.Fatalf("list builds = %d (%s), want 200 without GitHub dependencies", rec.Code, rec.Body.String())
			}
			body := decodeBody(t, rec)
			if body["repo"] != tc.displayName {
				t.Fatalf("repo = %v, want %q", body["repo"], tc.displayName)
			}
			if body["automaticDeploys"] != false || body["githubConfigured"] != false ||
				body["githubConnected"] != false {
				t.Fatalf("non-GitHub state = %+v, want automatic/configured/connected false", body)
			}
		})
	}
}

func TestManualDeployQueuesAppScopedDoorbell(t *testing.T) {
	app := &orkanov1alpha1.App{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: appsNamespace}, Spec: webAppSpec()}
	store := newFakeStore()
	s := apiServer(t, store, app)
	ck := authedSession(t, store)

	rec := apiReq(t, s, http.MethodPost, "/api/apps/web/deploy", nil, ck)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("deploy = %d (%s), want 202", rec.Code, rec.Body.String())
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.deliveries) != 1 {
		t.Fatalf("deliveries = %d, want one", len(store.deliveries))
	}
	row := store.deliveries[0]
	if row.Repo != app.Spec.Source.GitHub.Repo || !row.AppName.Valid || row.AppName.String != "web" {
		t.Fatalf("delivery = %+v, want repo pointer scoped to web", row)
	}
	if len(row.DeliveryID) != len(manualDeliveryPrefix)+32 {
		t.Fatalf("delivery ID = %q, want prefix + 128-bit hex", row.DeliveryID)
	}
}

func TestCreateAppQueuesInitialBuild(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := authedSession(t, store)

	rec := apiReq(t, s, http.MethodPost, "/api/apps", appCreateRequest{Name: "web", Spec: webAppSpec()}, ck)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", rec.Code, rec.Body.String())
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.deliveries) != 1 || store.deliveries[0].AppName.String != "web" {
		t.Fatalf("initial build deliveries = %+v, want one scoped to web", store.deliveries)
	}
}

func TestManualDeployQueueFailureIsClear(t *testing.T) {
	app := &orkanov1alpha1.App{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: appsNamespace}, Spec: webAppSpec()}
	store := newFakeStore()
	store.enqueueErr = errors.New("queue down")
	s := apiServer(t, store, app)
	ck := authedSession(t, store)

	rec := apiReq(t, s, http.MethodPost, "/api/apps/web/deploy", nil, ck)
	if rec.Code != http.StatusServiceUnavailable || decodeBody(t, rec)["error"] != "unavailable" {
		t.Fatalf("deploy failure = %d (%s), want 503 unavailable", rec.Code, rec.Body.String())
	}
}

func TestBuildLogsUseJobReferenceAndBuildkitContainer(t *testing.T) {
	app := &orkanov1alpha1.App{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: appsNamespace}, Spec: webAppSpec()}
	build := buildFixture("web-build", "web", "Succeeded", fixedNow())
	build.Status.JobRef = &orkanov1alpha1.JobReference{Namespace: buildsNamespace, Name: "web-build-job"}
	streamer := &fakePodStreamer{
		pods:    []string{"web-build-job-abcde"},
		content: map[string]string{"web-build-job-abcde": "#1 loading Dockerfile\n#2 DONE\n"},
	}
	store := newFakeStore()
	s := apiServer(t, store, app, build)
	s.cfg.PodLogs = streamer
	ck := authedSession(t, store)

	rec := apiReq(t, s, http.MethodGet, "/api/apps/web/builds/web-build/logs?follow=false", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("build logs = %d (%s)", rec.Code, rec.Body.String())
	}
	if ns, key, value := streamer.lastList(); ns != buildsNamespace || key != buildJobPodLabel || value != "web-build-job" {
		t.Fatalf("listed (%q,%q,%q), want Build Job pods", ns, key, value)
	}
	if namespace := streamer.lastStreamNamespace(); namespace != buildsNamespace {
		t.Fatalf("stream namespace = %q, want %q", namespace, buildsNamespace)
	}
	opts := streamer.recordedOpts()
	if len(opts) != 1 || opts[0].Container != buildContainerName {
		t.Fatalf("stream opts = %+v, want buildkit container", opts)
	}
	if lines := dataLines(t, parseSSE(t, rec.Body.String())); len(lines) != 2 {
		t.Fatalf("log lines = %v, want two historical BuildKit lines", lines)
	}
}

func TestBuildLogsCannotCrossApps(t *testing.T) {
	app := &orkanov1alpha1.App{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: appsNamespace}, Spec: webAppSpec()}
	build := buildFixture("api-build", "api", "Succeeded", fixedNow())
	build.Status.JobRef = &orkanov1alpha1.JobReference{Namespace: buildsNamespace, Name: "api-build-job"}
	store := newFakeStore()
	s := apiServer(t, store, app, build)
	ck := authedSession(t, store)

	rec := apiReq(t, s, http.MethodGet, "/api/apps/web/builds/api-build/logs", nil, ck)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-app logs = %d, want 404", rec.Code)
	}
}
