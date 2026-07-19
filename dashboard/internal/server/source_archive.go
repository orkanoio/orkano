package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"regexp"

	"github.com/go-chi/chi/v5"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
	"github.com/orkanoio/orkano/internal/features"
	"github.com/orkanoio/orkano/internal/sourcearchive"
)

var sourceArchiveNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,250}[.]zip$`)

func (s *Server) handleUploadSourceArchive(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	name := chi.URLParam(r, "name")
	if !validResourceName(name) {
		writeJSONError(w, http.StatusBadRequest, "invalid_name")
		return
	}
	if !s.cfg.Features.Enabled(features.SourceZip) {
		err := &features.DisabledError{IDs: []features.ID{features.SourceZip}}
		s.auditResult(r, user, "app.source.upload", name, err)
		s.writeFeatureDisabled(w, err)
		return
	}
	filename := r.Header.Get("X-Orkano-Filename")
	if !sourceArchiveNamePattern.MatchString(filename) {
		writeJSONError(w, http.StatusBadRequest, "invalid_filename")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (mediaType != "application/zip" && mediaType != "application/x-zip-compressed") {
		writeJSONError(w, http.StatusUnsupportedMediaType, "zip_required")
		return
	}
	if r.ContentLength > sourcearchive.MaxCompressedBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "archive_too_large")
		return
	}

	var app orkanov1alpha1.App
	if err := s.cfg.K8s.Get(r.Context(), client.ObjectKey{Namespace: appsNamespace, Name: name}, &app); err != nil {
		s.auditResult(r, user, "app.source.upload", name, err)
		s.writeK8sError(w, "apps.source.upload", err)
		return
	}

	tmp, err := os.CreateTemp("", "orkano-source-*.zip")
	if err != nil {
		s.log.Error("create source archive temp file failed", "app", name, "err", err)
		writeJSONError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	hash := sha256.New()
	r.Body = http.MaxBytesReader(w, r.Body, sourcearchive.MaxCompressedBytes)
	size, copyErr := io.Copy(io.MultiWriter(tmp, hash), r.Body)
	closeErr := tmp.Close()
	if copyErr != nil || closeErr != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(copyErr, &tooLarge) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "archive_too_large")
			return
		}
		s.log.Error("receive source archive failed", "app", name, "err", errors.Join(copyErr, closeErr))
		writeJSONError(w, http.StatusBadRequest, "invalid_archive")
		return
	}
	if size == 0 {
		writeJSONError(w, http.StatusUnprocessableEntity, "invalid_archive")
		return
	}
	if _, err := sourcearchive.Inspect(tmpName); err != nil {
		s.auditResult(r, user, "app.source.upload", name, err)
		writeJSONError(w, http.StatusUnprocessableEntity, "invalid_archive")
		return
	}

	digest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	// tmpName came directly from CreateTemp above and is never caller-selected.
	archive, err := os.Open(tmpName) //nolint:gosec
	if err != nil {
		s.log.Error("reopen source archive failed", "app", name, "err", err)
		writeJSONError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	err = s.cfg.Archives.Upload(r.Context(), name, filename, archive, size, digest)
	closeErr = archive.Close()
	err = errors.Join(err, closeErr)
	s.auditResult(r, user, "app.source.upload", name, err)
	if err != nil {
		s.log.Error("store source archive failed", "app", name, "err", err)
		writeJSONError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"digest": digest, "fileName": filename})
}
