package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
)

const maxInputFrameBytes = 64 << 10

type liveState struct {
	LeaseID   string    `json:"lease_id"`
	WorkerID  string    `json:"worker_id,omitempty"`
	Device    string    `json:"device,omitempty"`
	OS        string    `json:"os,omitempty"`
	Mode      string    `json:"mode,omitempty"`
	Session   string    `json:"session,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

func liveLeaseID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var live liveState
	if json.Unmarshal(raw, &live) != nil {
		return ""
	}
	return live.LeaseID
}

func verificationVerdict(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var metrics struct {
		Verification struct {
			Verdict string `json:"verdict"`
		} `json:"verification"`
	}
	if json.Unmarshal(raw, &metrics) != nil {
		return ""
	}
	return metrics.Verification.Verdict
}

func (s *Server) reportBuildLive(w http.ResponseWriter, r *http.Request) {
	var live liveState
	decoder := json.NewDecoder(io.LimitReader(r.Body, 8<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&live); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid live report"})
		return
	}
	if !validLeaseID(live.LeaseID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "lease_id is required"})
		return
	}
	live.UpdatedAt = time.Now().UTC()
	data, _ := json.Marshal(live)
	result, err := s.db.ExecContext(r.Context(), `
		UPDATE builds SET live=$1::jsonb, updated_at=now()
		 WHERE id=$2 AND status='running' AND kind='verify'`, string(data), chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "record live device failed"})
		return
	}
	if count, _ := result.RowsAffected(); count == 0 {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "build is not a running verification"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func validLeaseID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func (s *Server) liveBuild(w http.ResponseWriter, r *http.Request) (Build, liveState, bool) {
	build, err := s.fetchBuild(r.Context(), "WHERE install_token=$1 AND kind='verify'", chi.URLParam(r, "token"))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return Build{}, liveState{}, false
	}
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return Build{}, liveState{}, false
	}
	var live liveState
	if len(build.Live) > 0 {
		_ = json.Unmarshal(build.Live, &live)
	}
	return build, live, true
}

func (s *Server) viewPage(w http.ResponseWriter, r *http.Request) {
	build, live, ok := s.liveBuild(w, r)
	if !ok {
		return
	}
	document, err := schemaFS.ReadFile("viewer.html")
	if err != nil {
		http.Error(w, "viewer unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(document)
	_ = build
	_ = live
}

func (s *Server) viewInfo(w http.ResponseWriter, r *http.Request) {
	build, live, ok := s.liveBuild(w, r)
	if !ok {
		return
	}
	available := build.Status == "running" && live.LeaseID != ""
	reason := ""
	if build.Status != "running" {
		reason = "verification finished"
	} else if live.LeaseID == "" {
		reason = "no live device yet"
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"name":      build.Name,
		"status":    build.Status,
		"stage":     build.Stage,
		"device":    live.Device,
		"os":        live.OS,
		"mode":      live.Mode,
		"available": available,
		"reason":    reason,
		"viewer":    s.cfg.SimlockOperatorToken != "",
	})
}

func (s *Server) viewScreen(w http.ResponseWriter, r *http.Request) {
	build, live, ok := s.liveBuild(w, r)
	if !ok {
		return
	}
	if build.Status != "running" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "verification finished; the device was released"})
		return
	}
	if !validLeaseID(live.LeaseID) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "no live device yet"})
		return
	}
	if s.cfg.SimlockOperatorToken == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "live view is not configured"})
		return
	}
	target := s.cfg.SimlockGatewayURL + "/v1/leases/" + url.PathEscape(live.LeaseID) + "/stream"
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	request.Header.Set("Authorization", "Bearer "+s.cfg.SimlockOperatorToken)
	response, err := (&http.Client{}).Do(request)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "simlock gateway unreachable"})
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		log.Printf("view screen: gateway returned %s: %s", response.Status, strings.TrimSpace(string(data)))
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "simlock gateway refused the stream"})
		return
	}
	for _, header := range []string{"Content-Type", "Simlock-Stream-Codec", "Simlock-Stream-Fps", "Simlock-Stream-Width", "Simlock-Stream-Height"} {
		if value := response.Header.Get(header); value != "" {
			w.Header().Set(header, value)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	buffer := make([]byte, 32<<10)
	for {
		read, readErr := response.Body.Read(buffer)
		if read > 0 {
			if _, writeErr := w.Write(buffer[:read]); writeErr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			return
		}
	}
}

func (s *Server) viewInput(w http.ResponseWriter, r *http.Request) {
	build, live, ok := s.liveBuild(w, r)
	if !ok {
		return
	}
	if build.Status != "running" || !validLeaseID(live.LeaseID) {
		http.Error(w, "no live device", http.StatusConflict)
		return
	}
	if s.cfg.SimlockOperatorToken == "" {
		http.Error(w, "live view is not configured", http.StatusServiceUnavailable)
		return
	}
	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	client.SetReadLimit(maxInputFrameBytes)
	defer client.CloseNow()

	upstreamURL := strings.Replace(s.cfg.SimlockGatewayURL, "http://", "ws://", 1)
	upstreamURL = strings.Replace(upstreamURL, "https://", "wss://", 1)
	upstreamURL += "/v1/leases/" + url.PathEscape(live.LeaseID) + "/input"
	dialCtx, cancelDial := context.WithTimeout(r.Context(), 10*time.Second)
	upstream, _, err := websocket.Dial(dialCtx, upstreamURL, &websocket.DialOptions{
		HTTPHeader:      http.Header{"Authorization": {"Bearer " + s.cfg.SimlockOperatorToken}},
		CompressionMode: websocket.CompressionDisabled,
	})
	cancelDial()
	if err != nil {
		_ = client.Close(websocket.StatusInternalError, "simlock gateway unavailable")
		return
	}
	upstream.SetReadLimit(maxInputFrameBytes)
	defer upstream.CloseNow()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{}, 2)
	relay := func(source, destination *websocket.Conn) {
		defer func() { done <- struct{}{} }()
		for {
			messageType, data, readErr := source.Read(ctx)
			if readErr != nil {
				return
			}
			if writeErr := destination.Write(ctx, messageType, data); writeErr != nil {
				return
			}
		}
	}
	go relay(client, upstream)
	go relay(upstream, client)
	<-done
	if err := client.Close(websocket.StatusNormalClosure, ""); err != nil {
		_ = client.CloseNow()
	}
	if err := upstream.Close(websocket.StatusNormalClosure, ""); err != nil {
		_ = upstream.CloseNow()
	}
}
