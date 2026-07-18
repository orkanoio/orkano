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

type mongoResponse struct {
	Name              string                     `json:"name"`
	Namespace         string                     `json:"namespace"`
	CreationTimestamp metav1.Time                `json:"creationTimestamp"`
	Spec              orkanov1alpha1.MongoSpec   `json:"spec"`
	Status            orkanov1alpha1.MongoStatus `json:"status"`
	SecretKeys        []string                   `json:"secretKeys"`
}

func mongoToResponse(m *orkanov1alpha1.Mongo) mongoResponse {
	return mongoResponse{
		Name: m.Name, Namespace: m.Namespace, CreationTimestamp: m.CreationTimestamp, Spec: m.Spec, Status: m.Status, SecretKeys: connectionSecretKeys(),
	}
}

type mongoCreateRequest struct {
	Name string                   `json:"name"`
	Spec orkanov1alpha1.MongoSpec `json:"spec"`
}

type mongoUpdateRequest struct {
	Spec orkanov1alpha1.MongoSpec `json:"spec"`
}

func (s *Server) handleListMongo(w http.ResponseWriter, r *http.Request) {
	var list orkanov1alpha1.MongoList
	if err := s.cfg.ViewerClient.List(r.Context(), &list, client.InNamespace(appsNamespace)); err != nil {
		s.writeK8sError(w, "mongo.list", err)
		return
	}
	out := make([]mongoResponse, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, mongoToResponse(&list.Items[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (s *Server) handleGetMongo(w http.ResponseWriter, r *http.Request) {
	var mongo orkanov1alpha1.Mongo
	key := client.ObjectKey{Namespace: appsNamespace, Name: chi.URLParam(r, "name")}
	if err := s.cfg.ViewerClient.Get(r.Context(), key, &mongo); err != nil {
		s.writeK8sError(w, "mongo.get", err)
		return
	}
	writeJSON(w, http.StatusOK, mongoToResponse(&mongo))
}

func (s *Server) handleCreateMongo(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	var req mongoCreateRequest
	if !s.decodeAPIJSON(w, r, &req) {
		return
	}
	if !validResourceName(req.Name) {
		writeJSONError(w, http.StatusBadRequest, "invalid_name")
		return
	}
	if req.Spec.MongoExpress != nil && req.Spec.MongoExpress.Enabled {
		writeJSONError(w, http.StatusBadRequest, "mongo_express_requires_step_up")
		return
	}
	req.Spec.MongoExpress = nil

	s.nameMu.Lock()
	defer s.nameMu.Unlock()
	if kind, err := s.conflictingResourceKind(r.Context(), req.Name, "Mongo"); err != nil {
		s.auditResult(r, user, "mongo.create", req.Name, err)
		s.writeK8sError(w, "mongo.create", err)
		return
	} else if kind != "" {
		s.auditResult(r, user, "mongo.create", req.Name, errResourceNameInUse)
		writeNameInUse(w, kind)
		return
	}
	if taken, err := s.esoClaimsSecretName(r.Context(), req.Name); err != nil {
		s.auditResult(r, user, "mongo.create", req.Name, err)
		s.writeK8sError(w, "mongo.create", err)
		return
	} else if taken {
		s.auditResult(r, user, "mongo.create", req.Name, errResourceNameInUse)
		writeJSONError(w, http.StatusConflict, "name_conflict")
		return
	}

	mongo := &orkanov1alpha1.Mongo{
		ObjectMeta: metav1.ObjectMeta{Name: req.Name, Namespace: appsNamespace},
		Spec:       req.Spec,
	}
	err := s.cfg.K8s.Create(r.Context(), mongo)
	s.auditResult(r, user, "mongo.create", req.Name, err)
	if err != nil {
		s.writeK8sError(w, "mongo.create", err)
		return
	}
	writeJSON(w, http.StatusCreated, mongoToResponse(mongo))
}

func (s *Server) handleUpdateMongo(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	name := chi.URLParam(r, "name")
	var req mongoUpdateRequest
	if !s.decodeAPIJSON(w, r, &req) {
		return
	}
	var mongo orkanov1alpha1.Mongo
	key := client.ObjectKey{Namespace: appsNamespace, Name: name}
	if err := s.cfg.K8s.Get(r.Context(), key, &mongo); err != nil {
		s.auditResult(r, user, "mongo.update", name, err)
		s.writeK8sError(w, "mongo.update", err)
		return
	}
	if req.Spec.StorageSize == nil {
		req.Spec.StorageSize = mongo.Spec.StorageSize
	} else if mongo.Spec.StorageSize != nil && req.Spec.StorageSize.Cmp(*mongo.Spec.StorageSize) < 0 {
		writeJSONError(w, http.StatusBadRequest, "storage_shrink_forbidden")
		return
	}
	if req.Spec.MongoExpress != nil && req.Spec.MongoExpress.Enabled != mongo.MongoExpressEnabled() {
		writeJSONError(w, http.StatusBadRequest, "use_mongo_express_endpoint")
		return
	}
	if mongo.Spec.MongoExpress != nil {
		mongoExpress := *mongo.Spec.MongoExpress
		req.Spec.MongoExpress = &mongoExpress
	} else {
		req.Spec.MongoExpress = nil
	}
	mongo.Spec = req.Spec
	err := s.cfg.K8s.Update(r.Context(), &mongo)
	s.auditResult(r, user, "mongo.update", name, err)
	if err != nil {
		s.writeK8sError(w, "mongo.update", err)
		return
	}
	writeJSON(w, http.StatusOK, mongoToResponse(&mongo))
}

type mongoExpressUpdateRequest struct {
	Enabled bool `json:"enabled"`
}

func (s *Server) handleUpdateMongoExpress(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	name := chi.URLParam(r, "name")
	var req mongoExpressUpdateRequest
	if !s.decodeAPIJSON(w, r, &req) {
		return
	}
	var mongo orkanov1alpha1.Mongo
	key := client.ObjectKey{Namespace: appsNamespace, Name: name}
	if err := s.cfg.K8s.Get(r.Context(), key, &mongo); err != nil {
		s.auditResult(r, user, "mongo.express.update", name, err)
		s.writeK8sError(w, "mongo.express.update", err)
		return
	}
	if req.Enabled {
		mongo.Spec.MongoExpress = &orkanov1alpha1.MongoExpressSpec{Enabled: true}
	} else {
		mongo.Spec.MongoExpress = nil
	}
	err := s.cfg.K8s.Update(r.Context(), &mongo)
	action := "mongo.express.disable"
	if req.Enabled {
		action = "mongo.express.enable"
	}
	s.auditResult(r, user, action, name, err)
	if err != nil {
		s.writeK8sError(w, action, err)
		return
	}
	writeJSON(w, http.StatusOK, mongoToResponse(&mongo))
}

func (s *Server) handleMongoExpressRedirect(w http.ResponseWriter, r *http.Request) {
	// The destination is an absolute-path reference formed only by appending a
	// slash; it can never redirect to another host.
	//nolint:gosec // G710: r.URL.Path cannot turn the leading slash into an external redirect.
	http.Redirect(w, r, r.URL.Path+"/", http.StatusTemporaryRedirect)
}

func (s *Server) handleMongoExpressProxy(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	var mongo orkanov1alpha1.Mongo
	key := client.ObjectKey{Namespace: appsNamespace, Name: name}
	if err := s.cfg.ViewerClient.Get(r.Context(), key, &mongo); err != nil {
		s.writeK8sError(w, "mongo.express.open", err)
		return
	}
	if !mongo.MongoExpressEnabled() {
		writeJSONError(w, http.StatusConflict, "mongo_express_disabled")
		return
	}
	condition := meta.FindStatusCondition(mongo.Status.Conditions, orkanov1alpha1.ConditionMongoExpressReady)
	if condition == nil || condition.Status != metav1.ConditionTrue || mongo.Status.MongoExpressServiceName == "" {
		writeJSONError(w, http.StatusServiceUnavailable, "mongo_express_not_ready")
		return
	}
	serviceName := mongo.Status.MongoExpressServiceName
	if len(validation.IsDNS1035Label(serviceName)) != 0 {
		s.log.Error("operator reported an invalid Mongo Express Service name", "mongo", name, "service", serviceName)
		writeJSONError(w, http.StatusServiceUnavailable, "mongo_express_not_ready")
		return
	}
	prefix := "/api/mongo/" + name + "/express/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		writeJSONError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	target := &url.URL{
		Scheme: "http",
		Host:   serviceName + "." + appsNamespace + ".svc.cluster.local:8081",
	}
	expressSource := strings.TrimRight(s.dashboardBaseURL(r), "/") + prefix
	proxy := &httputil.ReverseProxy{Transport: s.cfg.MongoExpressTransport}
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
		resp.Header.Set("Content-Security-Policy", "default-src 'none'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'; script-src 'self' 'unsafe-inline' 'unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src "+expressSource+"; form-action "+expressSource)
		if location := resp.Header.Get("Location"); location != "" {
			parsed, err := url.Parse(location)
			if err == nil && parsed.IsAbs() && parsed.Host == target.Host {
				resp.Header.Set("Location", parsed.RequestURI())
			}
		}
		return nil
	}
	proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, err error) {
		s.log.Warn("Mongo Express proxy request failed", "mongo", name, "err", err)
		writeJSONError(rw, http.StatusBadGateway, "mongo_express_unavailable")
	}
	if r.Method == http.MethodGet && r.URL.Path == prefix {
		user, _ := userFromContext(r.Context())
		s.auditResult(r, user, "mongo.express.open", name, nil)
	}
	proxy.ServeHTTP(w, r)
}

func (s *Server) handleDeleteMongo(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	name := chi.URLParam(r, "name")
	mongo := &orkanov1alpha1.Mongo{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appsNamespace}}
	err := s.cfg.K8s.Delete(r.Context(), mongo)
	s.auditResult(r, user, "mongo.delete", name, err)
	if err != nil {
		s.writeK8sError(w, "mongo.delete", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
