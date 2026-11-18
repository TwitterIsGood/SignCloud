package control

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
	qrcode "github.com/skip2/go-qrcode"
)

//go:embed schema.sql client/signcloud_ai.py client/llms.txt client/docs/*.md viewer.html
var schemaFS embed.FS

// Embedded docs use this placeholder so the public source tree carries no deployment URL.
const docsPlaceholderURL = "https://build.example.com"

type Server struct {
	cfg         Config
	db          *sql.DB
	httpServer  *http.Server
	installTmpl *template.Template
	stop        chan struct{}
}

func New(cfg Config) (*Server, error) {
	if err := os.MkdirAll(filepath.Join(cfg.StorageDir, "sources"), 0o750); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(cfg.StorageDir, "artifacts"), 0o750); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(cfg.StorageDir, "blobs"), 0o750); err != nil {
		return nil, err
	}
	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := waitForDB(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	schema, _ := schemaFS.ReadFile("schema.sql")
	if _, err := db.ExecContext(ctx, string(schema)); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}

	s := &Server{cfg: cfg, db: db, installTmpl: template.Must(template.New("install").Parse(installPage)), stop: make(chan struct{})}
	go s.reapStaleBuilds()
	router := s.routes()
	s.httpServer = &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	return s, nil
}

func waitForDB(ctx context.Context, db *sql.DB) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := db.PingContext(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("database unavailable: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (s *Server) routes() http.Handler {
	r := chi.NewRouter()
	r.Use(s.recoverer)
	r.Get("/healthz", s.health)
	r.Get("/", s.serviceInfo)
	r.Get("/llms.txt", s.llmsTXT)
	r.Get("/docs/{name}", s.docsMD)
	r.Get("/client/signcloud_ai.py", s.aiClient)
	r.Get("/install/{token}", s.installPage)
	r.Get("/install/{token}/status", s.installStatus)
	r.Get("/install/{token}/qr.png", s.installQR)
	r.Get("/manifest/{token}.plist", s.manifest)
	r.Get("/artifacts/{token}", s.publicArtifact)
	r.Get("/artifacts/{token}/app.zip", s.publicAppZip)
	r.Get("/view/{token}", s.viewPage)
	r.Get("/view/{token}/info", s.viewInfo)
	r.Get("/view/{token}/screen", s.viewScreen)
	r.Get("/view/{token}/input", s.viewInput)

	r.Route("/v1", func(api chi.Router) {
		api.Group(func(user chi.Router) {
			user.Use(s.bearerAuth(s.cfg.apiTokens()...))
			user.Get("/builds", s.listBuilds)
			user.Post("/builds", s.createBuild)
			user.Get("/builds/{id}", s.getBuild)
			user.Post("/builds/{id}/cancel", s.cancelBuild)
			user.Get("/builds/{id}/source", s.downloadSource)
			user.Post("/uploads/plan", s.planUpload)
			user.Put("/blobs/batch", s.putBlobBatch)
			user.Put("/blobs/{hash}", s.putBlob)
			user.Post("/builds/incremental", s.createIncrementalBuild)
			user.Get("/devices", s.listDevices)
			user.Post("/devices", s.createDevice)
			user.Get("/devices/{id}", s.getDevice)
		})
		api.Group(func(agent chi.Router) {
			agent.Use(s.bearerAuth(s.cfg.AgentToken))
			agent.Post("/agent/claim", s.claimBuild)
			agent.Get("/agent/builds/{id}/source", s.downloadSource)
			agent.Get("/agent/builds/{id}/status", s.agentBuildStatus)
			agent.Get("/agent/blobs/{hash}", s.downloadBlob)
			agent.Post("/agent/builds/{id}/logs", s.appendLogs)
			agent.Post("/agent/builds/{id}/stage", s.updateBuildStage)
			agent.Post("/agent/builds/{id}/live", s.reportBuildLive)
			agent.Post("/agent/builds/{id}/complete", s.completeBuild)
			agent.Post("/agent/devices/claim", s.claimDevice)
			agent.Post("/agent/devices/{id}/complete", s.completeDevice)
		})
	})
	return r
}

func (s *Server) ListenAndServe() error { return s.httpServer.ListenAndServe() }
func (s *Server) Close() error {
	close(s.stop)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.httpServer.Shutdown(ctx)
	return s.db.Close()
}

// reapStaleBuilds returns builds whose worker stopped reporting progress to the
// queue so a recovered worker can pick them up again. Without it a worker that
// dies mid-build strands its job in 'running' forever.
func (s *Server) reapStaleBuilds() {
	timeout := s.cfg.StaleBuildTimeout
	if timeout <= 0 {
		timeout = 6 * time.Hour
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
		}
		res, err := s.db.Exec(`
			UPDATE builds
			   SET status = 'queued', claimed_by = '', stage = '', updated_at = now(),
			       logs = logs || $1
			 WHERE status = 'running' AND updated_at < now() - $2::interval`,
			fmt.Sprintf("\n[reaper] no worker progress for %s; requeued for retry\n", timeout),
			fmt.Sprintf("%d seconds", int(timeout.Seconds())))
		if err != nil {
			log.Printf("reaper: %v", err)
			continue
		}
		if count, _ := res.RowsAffected(); count > 0 {
			log.Printf("reaper: requeued %d stalled build(s)", count)
		}
	}
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.db.PingContext(r.Context()); err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) serviceInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"service":           "signcloud",
		"mode":              "api",
		"status":            "ok",
		"build_kinds":       []string{"simulator", "xcode", "expo", "resign", "macos", "resolve", "verify"},
		"macos_stages":      []string{"preparing", "generating_project", "building", "signing", "packaging", "notarizing", "stapling", "validating", "uploading"},
		"device_enrollment": s.cfg.DeviceEnrollmentEnabled,
		"endpoints": map[string]string{
			"health":       "/healthz",
			"builds":       "/v1/builds",
			"devices":      "/v1/devices",
			"ai_client":    "/client/signcloud_ai.py",
			"llms":         "/llms.txt",
			"docs":         "/docs/{name}.md",
			"install_page": "/install/{token}",
			"manifest":     "/manifest/{token}.plist",
			"artifact":     "/artifacts/{token}",
			"view":         "/view/{token}",
		},
	})
}

func (s *Server) llmsTXT(w http.ResponseWriter, r *http.Request) {
	document, err := schemaFS.ReadFile("client/llms.txt")
	if err != nil {
		http.Error(w, "documentation unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(s.renderDocs(document))
}

// renderDocs points embedded documentation at this deployment's public base URL.
func (s *Server) renderDocs(document []byte) []byte {
	base := strings.TrimRight(s.cfg.PublicBaseURL, "/")
	if base == "" {
		return document
	}
	return []byte(strings.ReplaceAll(string(document), docsPlaceholderURL, base))
}

// docsMD serves the reference documents linked from llms.txt. Names are flat and must end in .md.
func (s *Server) docsMD(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if !strings.HasSuffix(name, ".md") || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	document, err := schemaFS.ReadFile("client/docs/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(s.renderDocs(document))
}

func (s *Server) aiClient(w http.ResponseWriter, r *http.Request) {
	client, err := schemaFS.ReadFile("client/signcloud_ai.py")
	if err != nil {
		http.Error(w, "client unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/x-python; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="signcloud_ai.py"`)
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(client)
}

func (s *Server) bearerAuth(expected ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			matched := 0
			for _, candidate := range expected {
				if len(provided) == len(candidate) {
					matched |= subtle.ConstantTimeCompare([]byte(provided), []byte(candidate))
				}
			}
			if matched != 1 {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authSubjectKey{}, tokenSubject(provided))))
		})
	}
}

type authSubjectKey struct{}

func tokenSubject(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:8])
}

func requestSubject(r *http.Request) string {
	value, _ := r.Context().Value(authSubjectKey{}).(string)
	return value
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Printf("panic: %v", recovered)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) createBuild(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadMB*1024*1024)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid multipart upload: " + err.Error()})
		return
	}
	defer r.MultipartForm.RemoveAll()
	name := strings.TrimSpace(r.FormValue("name"))
	kind := strings.TrimSpace(r.FormValue("kind"))
	if name == "" || !validKind(kind) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name and kind (xcode, expo, resign, simulator, macos, resolve, verify) are required"})
		return
	}
	var spec json.RawMessage = []byte("{}")
	if raw := strings.TrimSpace(r.FormValue("spec")); raw != "" {
		if _, err := parseAndValidateBuildSpec(kind, []byte(raw)); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		spec = json.RawMessage(raw)
	} else if kind == "macos" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "macos spec is required"})
		return
	}
	file, header, err := r.FormFile("source")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "source file is required"})
		return
	}
	defer file.Close()

	id := randomID(16)
	token := randomID(24)
	safeName := sanitizeFilename(header.Filename)
	sourcePath := filepath.Join(s.cfg.StorageDir, "sources", id+"-"+safeName)
	if err := saveUpload(sourcePath, file); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	_, err = s.db.ExecContext(r.Context(), `
		INSERT INTO builds (id, name, kind, status, source_name, source_path, install_token, spec, source_mode)
		VALUES ($1, $2, $3, 'queued', $4, $5, $6, $7, 'archive')`, id, name, kind, safeName, sourcePath, token, spec)
	if err != nil {
		_ = os.Remove(sourcePath)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "create build failed"})
		return
	}
	build, err := s.fetchBuild(r.Context(), "WHERE id = $1", id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.decorate(&build, true)
	writeJSON(w, http.StatusCreated, build)
}

func saveUpload(path string, file multipart.File) error {
	dst, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o640)
	if err != nil {
		return err
	}
	defer dst.Close()
	_, err = io.Copy(dst, file)
	return err
}

func (s *Server) listBuilds(w http.ResponseWriter, r *http.Request) {
	query := selectBuild
	args := []any{}
	if status := strings.TrimSpace(r.URL.Query().Get("status")); status != "" {
		if !validStatus(status) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "status must be queued, running, succeeded, failed, or canceled"})
			return
		}
		args = append(args, status)
		query += fmt.Sprintf(" WHERE status = $%d", len(args))
	}
	limit := 200
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 1000 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "limit must be between 1 and 1000"})
			return
		}
		limit = value
	}
	query += fmt.Sprintf(" ORDER BY created_at DESC LIMIT %d", limit)
	rows, err := s.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	builds := make([]Build, 0)
	for rows.Next() {
		build, err := scanBuild(rows)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		s.decorate(&build, false)
		builds = append(builds, build)
	}
	writeJSON(w, http.StatusOK, builds)
}

func (s *Server) getBuild(w http.ResponseWriter, r *http.Request) {
	build, err := s.fetchBuild(r.Context(), "WHERE id = $1", chi.URLParam(r, "id"))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.decorate(&build, true)
	writeJSON(w, http.StatusOK, build)
}

func (s *Server) cancelBuild(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	result, err := s.db.ExecContext(r.Context(), `
		UPDATE builds
		   SET status = 'canceled', error = 'canceled by client', updated_at = now(),
		       logs = logs || E'\n[cancel] canceled by client\n'
		 WHERE id = $1 AND status IN ('queued', 'running')`, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "cancel build failed"})
		return
	}
	if count, _ := result.RowsAffected(); count == 0 {
		var status string
		if err := s.db.QueryRowContext(r.Context(), `SELECT status FROM builds WHERE id = $1`, id).Scan(&status); errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		} else if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusConflict, map[string]string{"error": "build is already " + status})
		return
	}
	build, err := s.fetchBuild(r.Context(), "WHERE id = $1", id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.decorate(&build, true)
	writeJSON(w, http.StatusOK, build)
}

func (s *Server) agentBuildStatus(w http.ResponseWriter, r *http.Request) {
	var status string
	err := s.db.QueryRowContext(r.Context(), `SELECT status FROM builds WHERE id = $1`, chi.URLParam(r, "id")).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": status})
}

func (s *Server) claimBuild(w http.ResponseWriter, r *http.Request) {
	var body struct {
		WorkerID string `json:"worker_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.WorkerID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "worker_id is required"})
		return
	}
	row := s.db.QueryRowContext(r.Context(), `
		WITH next AS (
			SELECT id FROM builds WHERE status = 'queued' ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1
		)
		UPDATE builds SET status = 'running', claimed_by = $1, updated_at = now()
		WHERE id = (SELECT id FROM next)
		RETURNING id, name, kind, status, source_name, artifact_name, install_token, spec, logs, error,
		          claimed_by, bundle_id, version, minimum_os, created_at, updated_at,
		          cache_key, source_mode, metrics, build_number, stage, architectures, signing,
		          signed, notarized, stapled, sha256, app_zip_name, live`, body.WorkerID)
	build, err := scanBuild(row)
	if errors.Is(err, sql.ErrNoRows) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.decorate(&build, true)
	build.SourceURL = "/v1/agent/builds/" + build.ID + "/source"
	writeJSON(w, http.StatusOK, build)
}

func (s *Server) downloadSource(w http.ResponseWriter, r *http.Request) {
	var sourcePath, sourceName string
	err := s.db.QueryRowContext(r.Context(), `SELECT source_path, source_name FROM builds WHERE id = $1`, chi.URLParam(r, "id")).Scan(&sourcePath, &sourceName)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, sourceName))
	http.ServeFile(w, r, sourcePath)
}

func (s *Server) appendLogs(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_, err = s.db.ExecContext(r.Context(), `UPDATE builds SET logs = logs || $1, updated_at = now() WHERE id = $2`, string(data), chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) updateBuildStage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Stage string `json:"stage"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid build stage"})
		return
	}
	var kind string
	err := s.db.QueryRowContext(r.Context(), `SELECT kind FROM builds WHERE id=$1 AND status='running'`, chi.URLParam(r, "id")).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "build is not running"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "update build stage failed"})
		return
	}
	if !containsString(buildStageOrder[kind], body.Stage) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid build stage for " + kind})
		return
	}
	result, err := s.db.ExecContext(r.Context(), `UPDATE builds SET stage=$1, updated_at=now() WHERE id=$2 AND status='running'`, body.Stage, chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "update build stage failed"})
		return
	}
	if count, _ := result.RowsAffected(); count == 0 {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "build is not running"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) completeBuild(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadMB*1024*1024)
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	defer r.MultipartForm.RemoveAll()
	id := chi.URLParam(r, "id")
	status := r.FormValue("status")
	if status != "succeeded" && status != "failed" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "status must be succeeded or failed"})
		return
	}
	var kind string
	err := s.db.QueryRowContext(r.Context(), `SELECT kind FROM builds WHERE id=$1 AND status='running'`, id).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "build is not running"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	artifactName, artifactPath, appZipName, appZipPath := "", "", "", ""
	if status == "succeeded" || kind == "verify" {
		file, header, fileErr := r.FormFile("artifact")
		if fileErr == nil {
			defer file.Close()
			artifactName = sanitizeFilename(header.Filename)
			artifactPath = filepath.Join(s.cfg.StorageDir, "artifacts", id+"-"+artifactName)
			if err := saveUpload(artifactPath, file); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			if file, header, appZipErr := r.FormFile("app_zip"); appZipErr == nil {
				defer file.Close()
				appZipName = sanitizeFilename(header.Filename)
				appZipPath = filepath.Join(s.cfg.StorageDir, "artifacts", id+"-app-"+appZipName)
				if err := saveUpload(appZipPath, file); err != nil {
					_ = os.Remove(artifactPath)
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "save app.zip failed"})
					return
				}
			}
		} else if status == "succeeded" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "artifact is required for succeeded builds"})
			return
		}
	}
	metrics := r.FormValue("metrics")
	if metrics == "" || !json.Valid([]byte(metrics)) {
		metrics = `{}`
	}
	architectures := r.FormValue("architectures")
	if architectures == "" || !json.Valid([]byte(architectures)) {
		architectures = `[]`
	}
	artifactSHA256 := ""
	if artifactPath != "" {
		var digestErr error
		artifactSHA256, digestErr = sha256Path(artifactPath)
		if digestErr != nil {
			_ = os.Remove(artifactPath)
			_ = os.Remove(appZipPath)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "calculate artifact SHA-256 failed"})
			return
		}
	}
	result, err := s.db.ExecContext(r.Context(), `
		UPDATE builds SET status=$1, artifact_name=$2, artifact_path=$3, error=$4,
		bundle_id=$5, version=$6, minimum_os=$7, metrics=metrics || $8::jsonb,
		app_zip_name=$9, app_zip_path=$10, build_number=$11, architectures=$12::jsonb,
		signing=$13, signed=$14, notarized=$15, stapled=$16, sha256=$17, updated_at=now()
		WHERE id=$18 AND status='running'`, status, artifactName, artifactPath, r.FormValue("error"),
		r.FormValue("bundle_id"), r.FormValue("version"), r.FormValue("minimum_os"), metrics,
		appZipName, appZipPath, r.FormValue("build_number"), architectures, r.FormValue("signing"),
		r.FormValue("signed") == "true", r.FormValue("notarized") == "true", r.FormValue("stapled") == "true",
		artifactSHA256, id)
	if err != nil {
		_ = os.Remove(artifactPath)
		_ = os.Remove(appZipPath)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if count, _ := result.RowsAffected(); count == 0 {
		_ = os.Remove(artifactPath)
		_ = os.Remove(appZipPath)
		writeJSON(w, http.StatusConflict, map[string]string{"error": "build is not running"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

const selectBuild = `SELECT id, name, kind, status, source_name, artifact_name, install_token, spec, logs, error,
claimed_by, bundle_id, version, minimum_os, created_at, updated_at, cache_key, source_mode, metrics,
build_number, stage, architectures, signing, signed, notarized, stapled, sha256, app_zip_name, live FROM builds`

type scanner interface{ Scan(...any) error }

func scanBuild(row scanner) (Build, error) {
	var b Build
	var architectures json.RawMessage
	var appZipName string
	err := row.Scan(&b.ID, &b.Name, &b.Kind, &b.Status, &b.SourceName, &b.ArtifactName, &b.InstallToken,
		&b.Spec, &b.Logs, &b.Error, &b.ClaimedBy, &b.BundleID, &b.Version, &b.MinimumOS, &b.CreatedAt, &b.UpdatedAt,
		&b.CacheKey, &b.SourceMode, &b.Metrics, &b.BuildNumber, &b.Stage, &architectures, &b.Signing,
		&b.Signed, &b.Notarized, &b.Stapled, &b.SHA256, &appZipName, &b.Live)
	if err == nil && len(architectures) > 0 {
		_ = json.Unmarshal(architectures, &b.Architectures)
	}
	return b, err
}

func (s *Server) fetchBuild(ctx context.Context, clause string, args ...any) (Build, error) {
	return scanBuild(s.db.QueryRowContext(ctx, selectBuild+" "+clause, args...))
}

func (s *Server) decorate(build *Build, includeSource bool) {
	build.MinimumSystemVersion = build.MinimumOS
	if build.Kind == "xcode" || build.Kind == "expo" || build.Kind == "resign" {
		build.InstallURL = s.cfg.PublicBaseURL + "/install/" + build.InstallToken
		build.QRURL = build.InstallURL + "/qr.png"
		build.ManifestURL = s.cfg.PublicBaseURL + "/manifest/" + build.InstallToken + ".plist"
		build.OTAURL = "itms-services://?action=download-manifest&url=" + url.QueryEscape(build.ManifestURL)
	}
	artifactAvailable := build.Status == "succeeded" || (build.Kind == "verify" && build.Status == "failed" && build.ArtifactName != "")
	if artifactAvailable {
		build.ArtifactURL = s.cfg.PublicBaseURL + "/artifacts/" + build.InstallToken
		var appZipPath string
		if err := s.db.QueryRow(`SELECT app_zip_path FROM builds WHERE id=$1`, build.ID).Scan(&appZipPath); err == nil && appZipPath != "" {
			build.AppZipURL = s.cfg.PublicBaseURL + "/artifacts/" + build.InstallToken + "/app.zip"
		}
	}
	if build.Kind == "verify" {
		if verdict := verificationVerdict(build.Metrics); verdict != "" {
			build.Verdict = verdict
		}
		if build.Status == "running" && liveLeaseID(build.Live) != "" {
			build.ViewURL = s.cfg.PublicBaseURL + "/view/" + build.InstallToken
		}
	}
	if includeSource {
		build.SourceURL = s.cfg.PublicBaseURL + "/v1/builds/" + build.ID + "/source"
	}
}

func sha256Path(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func randomID(bytes int) string {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		panic(err)
	}
	return hex.EncodeToString(value)
}

func sanitizeFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r == '_' || r == '-' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return r
		}
		return '_'
	}, name)
	if name == "" || name == "." {
		return "upload.bin"
	}
	return name
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *Server) publicArtifact(w http.ResponseWriter, r *http.Request) {
	var path, name string
	err := s.db.QueryRowContext(r.Context(), `SELECT artifact_path, artifact_name FROM builds
		WHERE install_token=$1 AND artifact_path <> ''
		  AND (status='succeeded' OR (kind='verify' AND status='failed'))`, chi.URLParam(r, "token")).Scan(&path, &name)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, name))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("ETag", `"`+chi.URLParam(r, "token")+`"`)
	http.ServeFile(w, r, path)
}

func (s *Server) publicAppZip(w http.ResponseWriter, r *http.Request) {
	var path, name string
	err := s.db.QueryRowContext(r.Context(), `SELECT app_zip_path, app_zip_name FROM builds
		WHERE install_token=$1 AND app_zip_path <> ''
		  AND (status='succeeded' OR (kind='verify' AND status='failed'))`, chi.URLParam(r, "token")).Scan(&path, &name)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, name))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFile(w, r, path)
}

func (s *Server) installPage(w http.ResponseWriter, r *http.Request) {
	build, err := s.fetchBuild(r.Context(), "WHERE install_token=$1", chi.URLParam(r, "token"))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if build.Kind == "simulator" || build.Kind == "resolve" || build.Kind == "verify" {
		http.NotFound(w, r)
		return
	}
	s.decorate(&build, false)
	showDetails := build.Status == "failed" || build.Status == "canceled" || r.URL.Query().Get("logs") != ""
	var logsTail string
	if showDetails {
		logsTail = tailText(build.Logs, installLogTailLimit)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.installTmpl.Execute(w, installPageData{
		Build:       build,
		SafeOTAURL:  template.URL(build.OTAURL),
		StatusText:  buildStatusText(build.Status),
		Progress:    buildProgress(build),
		LogsTail:    logsTail,
		ElapsedText: elapsedText(build),
		ShowDetails: showDetails,
	})
}

func (s *Server) installStatus(w http.ResponseWriter, r *http.Request) {
	var build Build
	err := s.db.QueryRowContext(r.Context(), `SELECT kind, status, stage, error, created_at, updated_at, right(logs, $2)
		FROM builds WHERE install_token=$1`, chi.URLParam(r, "token"), installLogTailLimit).
		Scan(&build.Kind, &build.Status, &build.Stage, &build.Error, &build.CreatedAt, &build.UpdatedAt, &build.Logs)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if build.Kind == "simulator" || build.Kind == "resolve" || build.Kind == "verify" {
		http.NotFound(w, r)
		return
	}
	var logs string
	if r.URL.Query().Get("logs") != "" {
		logs = build.Logs
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, installStatusData{
		Status:    build.Status,
		Error:     build.Error,
		UpdatedAt: build.UpdatedAt,
		Logs:      logs,
		Progress:  buildProgress(build),
	})
}

type installPageData struct {
	Build
	SafeOTAURL  template.URL
	StatusText  string
	Progress    installProgress
	LogsTail    string
	ElapsedText string
	ShowDetails bool
}

type installStatusData struct {
	Status    string          `json:"status"`
	Error     string          `json:"error,omitempty"`
	UpdatedAt time.Time       `json:"updated_at"`
	Logs      string          `json:"logs,omitempty"`
	Progress  installProgress `json:"progress"`
}

type installStage struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	State string `json:"state"`
}

type installProgress struct {
	Percent    *int           `json:"percent"`
	Stage      string         `json:"stage"`
	Label      string         `json:"-"`
	Width      int            `json:"-"`
	HasPercent bool           `json:"-"`
	Busy       bool           `json:"-"`
	Stages     []installStage `json:"stages"`
}

const installLogTailLimit = 64 << 10

var buildStageOrder = map[string][]string{
	"xcode":     {"preparing", "building", "packaging", "uploading"},
	"expo":      {"preparing", "building", "packaging", "uploading"},
	"simulator": {"preparing", "building", "packaging", "uploading"},
	"resign":    {"preparing", "signing", "packaging", "uploading"},
	"macos":     {"preparing", "generating_project", "building", "signing", "packaging", "notarizing", "stapling", "validating", "uploading"},
	"verify":    {"preparing", "building", "leasing", "installing", "verifying", "packaging", "uploading"},
}

var buildStagePercent = map[string]map[string]int{
	"xcode":     {"preparing": 8, "building": 55, "packaging": 86, "uploading": 96},
	"expo":      {"preparing": 8, "building": 55, "packaging": 86, "uploading": 96},
	"simulator": {"preparing": 8, "building": 62, "packaging": 90, "uploading": 96},
	"resign":    {"preparing": 10, "signing": 58, "packaging": 86, "uploading": 96},
	"macos": {
		"preparing": 5, "generating_project": 12, "building": 45, "signing": 62, "packaging": 72,
		"notarizing": 82, "stapling": 88, "validating": 93, "uploading": 97,
	},
	"verify": {"preparing": 8, "building": 45, "leasing": 58, "installing": 68, "verifying": 82, "packaging": 92, "uploading": 97},
}

var buildStageLabels = map[string]string{
	"preparing":          "准备",
	"generating_project": "生成工程",
	"building":           "编译",
	"signing":            "签名",
	"packaging":          "打包",
	"notarizing":         "公证",
	"stapling":           "装订",
	"validating":         "校验",
	"leasing":            "租用设备",
	"installing":         "安装",
	"verifying":          "验证",
	"uploading":          "上传",
}

func buildStatusText(status string) string {
	switch status {
	case "queued":
		return "排队中"
	case "running":
		return "构建中"
	case "succeeded":
		return "已完成"
	case "failed":
		return "构建失败"
	case "canceled":
		return "已取消"
	}
	return status
}

func buildProgress(build Build) installProgress {
	order := buildStageOrder[build.Kind]
	stage := build.Stage
	if stage == "" && build.Status == "running" {
		stage = inferStage(build.Logs)
	}
	if !containsString(order, stage) {
		stage = ""
	}
	progress := installProgress{Stage: stage, Stages: make([]installStage, 0, len(order))}
	current := -1
	for i, key := range order {
		if key == stage {
			current = i
		}
	}
	switch build.Status {
	case "succeeded":
		percent := 100
		progress.Percent = &percent
		progress.Label = "已完成"
	case "failed":
		progress.Label = "构建失败"
		if stage != "" {
			if percent, ok := buildStagePercent[build.Kind][stage]; ok {
				progress.Percent = &percent
			}
		}
	case "canceled":
		progress.Label = "已取消"
	case "queued":
		progress.Label = "排队中"
	default:
		progress.Label = "进行中"
		if stage != "" {
			if percent, ok := buildStagePercent[build.Kind][stage]; ok {
				progress.Percent = &percent
				progress.Label = buildStageLabels[stage]
			}
		}
	}
	if progress.Percent != nil {
		progress.Width = *progress.Percent
		progress.HasPercent = true
	}
	progress.Busy = build.Status == "queued" || build.Status == "running"
	for i, key := range order {
		state := "pending"
		switch build.Status {
		case "succeeded":
			state = "done"
		case "failed":
			if current >= 0 && i < current {
				state = "done"
			} else if i == current {
				state = "failed"
			}
		default:
			if current >= 0 && i < current {
				state = "done"
			} else if i == current {
				state = "current"
			}
		}
		progress.Stages = append(progress.Stages, installStage{Key: key, Label: buildStageLabels[key], State: state})
	}
	return progress
}

// inferStage uses the last phase marker in the log for builds whose worker does
// not report stages, so the pending page still shows meaningful progress.
func inferStage(logs string) string {
	markers := map[string]string{
		"xcodebuild": "building",
		"codesign":   "signing",
		"ditto":      "packaging",
	}
	stage := ""
	position := -1
	for marker, candidate := range markers {
		if index := strings.LastIndex(logs, marker); index > position {
			position = index
			stage = candidate
		}
	}
	return stage
}

func containsString(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func tailText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	start := len(text) - limit
	if index := strings.IndexByte(text[start:], '\n'); index >= 0 {
		start += index + 1
	}
	return strings.ToValidUTF8(text[start:], "")
}

func elapsedText(build Build) string {
	end := time.Now()
	if build.Status == "succeeded" || build.Status == "failed" || build.Status == "canceled" {
		end = build.UpdatedAt
	}
	seconds := int64(end.Sub(build.CreatedAt).Seconds())
	if seconds < 0 {
		seconds = 0
	}
	minutes := seconds / 60
	hours := minutes / 60
	switch {
	case hours > 0:
		return fmt.Sprintf("已用时 %d 小时 %d 分", hours, minutes%60)
	case minutes > 0:
		return fmt.Sprintf("已用时 %d 分 %d 秒", minutes, seconds%60)
	default:
		return fmt.Sprintf("已用时 %d 秒", seconds)
	}
}

func (s *Server) installQR(w http.ResponseWriter, r *http.Request) {
	url := s.cfg.PublicBaseURL + "/install/" + chi.URLParam(r, "token")
	png, err := qrcode.Encode(url, qrcode.Medium, 384)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(png)
}

func (s *Server) manifest(w http.ResponseWriter, r *http.Request) {
	build, err := s.fetchBuild(r.Context(), "WHERE install_token=$1 AND status='succeeded' AND kind NOT IN ('simulator', 'resolve', 'verify')", chi.URLParam(r, "token"))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	bundleID := build.BundleID
	if bundleID == "" {
		bundleID = "com.signcloud.unknown"
	}
	version := build.Version
	if version == "" {
		version = "1.0"
	}
	w.Header().Set("Content-Type", "application/xml")
	_, _ = fmt.Fprintf(w, manifestTemplate,
		template.HTMLEscapeString(s.cfg.PublicBaseURL+"/artifacts/"+build.InstallToken),
		template.HTMLEscapeString(bundleID), template.HTMLEscapeString(version), template.HTMLEscapeString(build.Name))
}
