package control

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"
)

var deviceUDIDPattern = regexp.MustCompile(`^(?:[0-9A-Fa-f]{40}|[0-9A-Fa-f]{8}-[0-9A-Fa-f]{16})$`)

const selectDevice = `SELECT id, requested_by, udid, name, platform, status, apple_device_id, error, claimed_by, created_at, updated_at FROM devices`

func (s *Server) createDevice(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.DeviceEnrollmentEnabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "device enrollment is not enabled on this SignCloud deployment"})
		return
	}
	var input struct {
		UDID     string `json:"udid"`
		Name     string `json:"name"`
		Platform string `json:"platform"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON request"})
		return
	}
	input.UDID = strings.ToUpper(strings.TrimSpace(input.UDID))
	input.Name = strings.TrimSpace(input.Name)
	input.Platform = strings.ToUpper(strings.TrimSpace(input.Platform))
	if input.Platform == "" {
		input.Platform = "IOS"
	}
	if !deviceUDIDPattern.MatchString(input.UDID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "udid must be a valid 40-character or modern hyphenated Apple device UDID"})
		return
	}
	if input.Name == "" || len(input.Name) > 100 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "device name is required and must be at most 100 characters"})
		return
	}
	if input.Platform != "IOS" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "only IOS devices are currently supported"})
		return
	}
	subject := requestSubject(r)
	var existingOwner string
	err := s.db.QueryRowContext(r.Context(), `SELECT requested_by FROM devices WHERE udid=$1`, input.UDID).Scan(&existingOwner)
	if err == nil && existingOwner != subject {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this device is already managed by another API token"})
		return
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "check device failed"})
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		var count int
		if countErr := s.db.QueryRowContext(r.Context(), `SELECT count(*) FROM devices WHERE requested_by=$1`, subject).Scan(&count); countErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "check device quota failed"})
			return
		}
		if count >= s.cfg.MaxDevicesPerToken {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "device quota reached for this API token"})
			return
		}
	}

	id := randomID(16)
	row := s.db.QueryRowContext(r.Context(), `
		INSERT INTO devices (id, requested_by, udid, name, platform)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (udid) DO UPDATE SET
			name=EXCLUDED.name,
			status=CASE WHEN devices.status='registered' THEN 'registered' ELSE 'queued' END,
			error=CASE WHEN devices.status='registered' THEN devices.error ELSE '' END,
			claimed_by=CASE WHEN devices.status='registered' THEN devices.claimed_by ELSE '' END,
			updated_at=now()
		WHERE devices.requested_by=EXCLUDED.requested_by
		RETURNING id, requested_by, udid, name, platform, status, apple_device_id, error, claimed_by, created_at, updated_at`,
		id, subject, input.UDID, input.Name, input.Platform)
	device, err := scanDevice(row)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this device is already managed by another API token"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "create device failed"})
		return
	}
	writeJSON(w, http.StatusAccepted, publicDevice(device))
}

func (s *Server) listDevices(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), selectDevice+` WHERE requested_by=$1 ORDER BY created_at DESC`, requestSubject(r))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "list devices failed"})
		return
	}
	defer rows.Close()
	devices := make([]Device, 0)
	for rows.Next() {
		device, scanErr := scanDevice(rows)
		if scanErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "read devices failed"})
			return
		}
		devices = append(devices, publicDevice(device))
	}
	writeJSON(w, http.StatusOK, devices)
}

func (s *Server) getDevice(w http.ResponseWriter, r *http.Request) {
	device, err := scanDevice(s.db.QueryRowContext(r.Context(), selectDevice+` WHERE id=$1 AND requested_by=$2`, chi.URLParam(r, "id"), requestSubject(r)))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "get device failed"})
		return
	}
	writeJSON(w, http.StatusOK, publicDevice(device))
}

func (s *Server) claimDevice(w http.ResponseWriter, r *http.Request) {
	var input struct {
		WorkerID string `json:"worker_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || strings.TrimSpace(input.WorkerID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "worker_id is required"})
		return
	}
	row := s.db.QueryRowContext(r.Context(), `
		WITH next AS (
			SELECT id FROM devices
			WHERE status='queued' OR (status='running' AND updated_at < now() - interval '10 minutes')
			ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1
		)
		UPDATE devices SET status='running', claimed_by=$1, updated_at=now()
		WHERE id=(SELECT id FROM next)
		RETURNING id, requested_by, udid, name, platform, status, apple_device_id, error, claimed_by, created_at, updated_at`, input.WorkerID)
	device, err := scanDevice(row)
	if errors.Is(err, sql.ErrNoRows) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "claim device failed"})
		return
	}
	writeJSON(w, http.StatusOK, device)
}

func (s *Server) completeDevice(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Status        string `json:"status"`
		AppleDeviceID string `json:"apple_device_id"`
		Error         string `json:"error"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON request"})
		return
	}
	if input.Status != "registered" && input.Status != "failed" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "status must be registered or failed"})
		return
	}
	result, err := s.db.ExecContext(r.Context(), `UPDATE devices SET status=$1, apple_device_id=$2, error=$3, updated_at=now() WHERE id=$4 AND status='running'`,
		input.Status, input.AppleDeviceID, input.Error, chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "complete device failed"})
		return
	}
	if count, _ := result.RowsAffected(); count == 0 {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "device is not running"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func scanDevice(row scanner) (Device, error) {
	var device Device
	err := row.Scan(&device.ID, &device.RequestedBy, &device.UDID, &device.Name, &device.Platform, &device.Status,
		&device.AppleDeviceID, &device.Error, &device.ClaimedBy, &device.CreatedAt, &device.UpdatedAt)
	return device, err
}

func publicDevice(device Device) Device {
	device.MaskedUDID = maskUDID(device.UDID)
	device.UDID = ""
	device.RequestedBy = ""
	return device
}

func maskUDID(udid string) string {
	if len(udid) <= 12 {
		return "***"
	}
	return udid[:8] + "..." + udid[len(udid)-6:]
}
