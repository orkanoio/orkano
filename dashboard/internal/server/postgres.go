package server

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

// postgresResponse is the read DTO for a catalog Postgres: identity, the spec
// (version + storageSize), and the operator-owned status (Ready + the produced
// connection-Secret name — never a value, INV-03).
type postgresResponse struct {
	Name              string                        `json:"name"`
	Namespace         string                        `json:"namespace"`
	CreationTimestamp metav1.Time                   `json:"creationTimestamp"`
	Spec              orkanov1alpha1.PostgresSpec   `json:"spec"`
	Status            orkanov1alpha1.PostgresStatus `json:"status"`
	SecretKeys        []string                      `json:"secretKeys"`
}

func postgresToResponse(p *orkanov1alpha1.Postgres) postgresResponse {
	return postgresResponse{
		Name:              p.Name,
		Namespace:         p.Namespace,
		CreationTimestamp: p.CreationTimestamp,
		Spec:              p.Spec,
		Status:            p.Status,
		SecretKeys:        connectionSecretKeys(),
	}
}

func connectionSecretKeys() []string {
	return []string{
		orkanov1alpha1.SecretKeyURI,
		orkanov1alpha1.SecretKeyHost,
		orkanov1alpha1.SecretKeyPort,
		orkanov1alpha1.SecretKeyDatabase,
		orkanov1alpha1.SecretKeyUsername,
		orkanov1alpha1.SecretKeyPassword,
	}
}

type postgresCreateRequest struct {
	Name string                      `json:"name"`
	Spec orkanov1alpha1.PostgresSpec `json:"spec"`
}

type postgresUpdateRequest struct {
	Spec orkanov1alpha1.PostgresSpec `json:"spec"`
}

func (s *Server) handleListPostgres(w http.ResponseWriter, r *http.Request) {
	var list orkanov1alpha1.PostgresList
	if err := s.cfg.ViewerClient.List(r.Context(), &list, client.InNamespace(appsNamespace)); err != nil {
		s.writeK8sError(w, "postgres.list", err)
		return
	}
	out := make([]postgresResponse, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, postgresToResponse(&list.Items[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (s *Server) handleGetPostgres(w http.ResponseWriter, r *http.Request) {
	var p orkanov1alpha1.Postgres
	key := client.ObjectKey{Namespace: appsNamespace, Name: chi.URLParam(r, "name")}
	if err := s.cfg.ViewerClient.Get(r.Context(), key, &p); err != nil {
		s.writeK8sError(w, "postgres.get", err)
		return
	}
	writeJSON(w, http.StatusOK, postgresToResponse(&p))
}

func (s *Server) handleCreatePostgres(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	var req postgresCreateRequest
	if !s.decodeAPIJSON(w, r, &req) {
		return
	}
	if !validResourceName(req.Name) {
		writeJSONError(w, http.StatusBadRequest, "invalid_name")
		return
	}
	if req.Spec.Pgweb != nil && req.Spec.Pgweb.Enabled {
		writeJSONError(w, http.StatusBadRequest, "pgweb_requires_step_up")
		return
	}
	req.Spec.Pgweb = nil
	s.nameMu.Lock()
	defer s.nameMu.Unlock()
	if kind, err := s.conflictingResourceKind(r.Context(), req.Name, "Postgres"); err != nil {
		s.auditResult(r, user, "postgres.create", req.Name, err)
		s.writeK8sError(w, "postgres.create", err)
		return
	} else if kind != "" {
		s.auditResult(r, user, "postgres.create", req.Name, errResourceNameInUse)
		writeNameInUse(w, kind)
		return
	}
	// The connection Secret is named after this object (ADR-0014), so a name
	// an ESO sync target or a store's credentials Secret already claims would
	// collide at the Secret layer — the reconciler would refuse it onto
	// ProvisionFailed; refuse earlier with a clean 409 (ADR-0018, the mirror
	// of the vault API's collision checks).
	if taken, err := s.esoClaimsSecretName(r.Context(), req.Name); err != nil {
		s.auditResult(r, user, "postgres.create", req.Name, err)
		s.writeK8sError(w, "postgres.create", err)
		return
	} else if taken {
		s.auditResult(r, user, "postgres.create", req.Name, errResourceNameInUse)
		writeJSONError(w, http.StatusConflict, "name_conflict")
		return
	}
	p := &orkanov1alpha1.Postgres{
		ObjectMeta: metav1.ObjectMeta{Name: req.Name, Namespace: appsNamespace},
		Spec:       req.Spec,
	}
	err := s.cfg.K8s.Create(r.Context(), p)
	s.auditResult(r, user, "postgres.create", req.Name, err)
	if err != nil {
		s.writeK8sError(w, "postgres.create", err)
		return
	}
	writeJSON(w, http.StatusCreated, postgresToResponse(p))
}

func (s *Server) handleUpdatePostgres(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	name := chi.URLParam(r, "name")
	var req postgresUpdateRequest
	if !s.decodeAPIJSON(w, r, &req) {
		return
	}
	// Read-modify-write on the SA client (the write path); never the viewer.
	var p orkanov1alpha1.Postgres
	key := client.ObjectKey{Namespace: appsNamespace, Name: name}
	if err := s.cfg.K8s.Get(r.Context(), key, &p); err != nil {
		s.auditResult(r, user, "postgres.update", name, err)
		s.writeK8sError(w, "postgres.update", err)
		return
	}
	// storageSize is grow-only, but the apiserver does NOT enforce it (native PVC
	// semantics, ADR-0014) — only the reconciler does, onto Ready=ProvisionFailed,
	// which is the backstop. Refuse a shrink here for a clean 400; an omitted size
	// preserves the current value rather than letting the schema default re-shrink
	// it. A stored object's storageSize is non-nil (the apiserver defaults it at
	// create), so the nil short-circuit below only skips the compare in tests,
	// where the fake client applies no defaults. Version immutability stays the
	// apiserver's job (a change passes through as a 422).
	if req.Spec.StorageSize == nil {
		req.Spec.StorageSize = p.Spec.StorageSize
	} else if p.Spec.StorageSize != nil && req.Spec.StorageSize.Cmp(*p.Spec.StorageSize) < 0 {
		writeJSONError(w, http.StatusBadRequest, "storage_shrink_forbidden")
		return
	}
	if req.Spec.Pgweb != nil && req.Spec.Pgweb.Enabled != p.PgwebEnabled() {
		writeJSONError(w, http.StatusBadRequest, "use_pgweb_endpoint")
		return
	}
	if p.Spec.Pgweb != nil {
		pgweb := *p.Spec.Pgweb
		req.Spec.Pgweb = &pgweb
	} else {
		req.Spec.Pgweb = nil
	}
	p.Spec = req.Spec
	err := s.cfg.K8s.Update(r.Context(), &p)
	s.auditResult(r, user, "postgres.update", name, err)
	if err != nil {
		s.writeK8sError(w, "postgres.update", err)
		return
	}
	writeJSON(w, http.StatusOK, postgresToResponse(&p))
}

type pgwebUpdateRequest struct {
	Enabled bool `json:"enabled"`
}

func (s *Server) handleUpdatePgweb(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	name := chi.URLParam(r, "name")
	var req pgwebUpdateRequest
	if !s.decodeAPIJSON(w, r, &req) {
		return
	}
	var pg orkanov1alpha1.Postgres
	key := client.ObjectKey{Namespace: appsNamespace, Name: name}
	if err := s.cfg.K8s.Get(r.Context(), key, &pg); err != nil {
		s.auditResult(r, user, "postgres.pgweb.update", name, err)
		s.writeK8sError(w, "postgres.pgweb.update", err)
		return
	}
	if req.Enabled {
		pg.Spec.Pgweb = &orkanov1alpha1.PgwebSpec{Enabled: true}
	} else {
		pg.Spec.Pgweb = nil
	}
	err := s.cfg.K8s.Update(r.Context(), &pg)
	action := "postgres.pgweb.disable"
	if req.Enabled {
		action = "postgres.pgweb.enable"
	}
	s.auditResult(r, user, action, name, err)
	if err != nil {
		s.writeK8sError(w, action, err)
		return
	}
	writeJSON(w, http.StatusOK, postgresToResponse(&pg))
}

func (s *Server) handlePgwebRedirect(w http.ResponseWriter, r *http.Request) {
	// The destination is an absolute-path reference formed only by appending a
	// slash; it cannot redirect to another host.
	//nolint:gosec // G710: r.URL.Path cannot turn the leading slash into an external redirect.
	http.Redirect(w, r, r.URL.Path+"/", http.StatusTemporaryRedirect)
}

func (s *Server) handlePgwebProxy(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	var pg orkanov1alpha1.Postgres
	key := client.ObjectKey{Namespace: appsNamespace, Name: name}
	if err := s.cfg.ViewerClient.Get(r.Context(), key, &pg); err != nil {
		s.writeK8sError(w, "postgres.pgweb.open", err)
		return
	}
	if !pg.PgwebEnabled() {
		writeJSONError(w, http.StatusConflict, "pgweb_disabled")
		return
	}
	condition := meta.FindStatusCondition(pg.Status.Conditions, orkanov1alpha1.ConditionPgwebReady)
	if condition == nil || condition.Status != metav1.ConditionTrue || pg.Status.PgwebServiceName == "" {
		writeJSONError(w, http.StatusServiceUnavailable, "pgweb_not_ready")
		return
	}
	serviceName := pg.Status.PgwebServiceName
	if len(validation.IsDNS1035Label(serviceName)) != 0 {
		s.log.Error("operator reported an invalid Pgweb Service name", "postgres", name, "service", serviceName)
		writeJSONError(w, http.StatusServiceUnavailable, "pgweb_not_ready")
		return
	}
	prefix := "/api/postgres/" + name + "/pgweb/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		writeJSONError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	target := &url.URL{
		Scheme: "http",
		Host:   serviceName + "." + appsNamespace + ".svc.cluster.local:8081",
	}
	pgwebSource := strings.TrimRight(s.dashboardBaseURL(r), "/") + prefix
	proxy := &httputil.ReverseProxy{Transport: s.cfg.PgwebTransport}
	proxy.Rewrite = func(request *httputil.ProxyRequest) {
		request.SetURL(target)
		request.Out.Host = target.Host
		request.Out.Header.Del("Authorization")
		request.Out.Header.Del("Cookie")
		request.Out.Header.Del("Forwarded")
		request.Out.Header.Del("X-Forwarded-For")
		request.Out.Header.Del("X-Forwarded-Host")
		request.Out.Header.Del("X-Forwarded-Proto")
		request.Out.Header.Set("X-Forwarded-Prefix", prefix)
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		resp.Header.Del("Set-Cookie")
		resp.Header.Del("WWW-Authenticate")
		resp.Header.Set("Cache-Control", "no-store")
		resp.Header.Set("X-Content-Type-Options", "nosniff")
		resp.Header.Set("X-Frame-Options", "DENY")
		resp.Header.Set("Referrer-Policy", "no-referrer")
		resp.Header.Set("Permissions-Policy", "camera=(), geolocation=(), microphone=(), payment=(), usb=()")
		resp.Header.Set("Content-Security-Policy", "default-src 'none'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'; script-src 'self' 'unsafe-inline' 'unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src "+pgwebSource+"; form-action "+pgwebSource)
		if location := resp.Header.Get("Location"); location != "" {
			parsed, err := url.Parse(location)
			if err == nil && parsed.IsAbs() && parsed.Host == target.Host {
				resp.Header.Set("Location", parsed.RequestURI())
			}
		}
		return nil
	}
	proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, err error) {
		s.log.Warn("Pgweb proxy request failed", "postgres", name, "err", err)
		writeJSONError(rw, http.StatusBadGateway, "pgweb_unavailable")
	}
	if r.Method == http.MethodGet && r.URL.Path == prefix {
		user, _ := userFromContext(r.Context())
		s.auditResult(r, user, "postgres.pgweb.open", name, nil)
	}
	proxy.ServeHTTP(w, r)
}

func (s *Server) handleDeletePostgres(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	name := chi.URLParam(r, "name")
	p := &orkanov1alpha1.Postgres{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appsNamespace}}
	err := s.cfg.K8s.Delete(r.Context(), p)
	s.auditResult(r, user, "postgres.delete", name, err)
	if err != nil {
		s.writeK8sError(w, "postgres.delete", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
