package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultVerifyDevice  = "iPhone 16"
	defaultVerifyTimeout = 900 * time.Second
	verifyStepTailBytes  = 2000
)

// verifyFailureError carries the evidence gathered before a verification failed,
// so the agent can still upload screenshots even though the build is reported as
// failed.
type verifyFailureError struct {
	err      error
	evidence string
	metadata ArtifactMetadata
}

func (e *verifyFailureError) Error() string { return e.err.Error() }
func (e *verifyFailureError) Unwrap() error { return e.err }

type leaseDetail struct {
	ID       string `json:"id"`
	Platform string `json:"platform"`
	Device   string `json:"device"`
	OS       string `json:"os"`
	UDID     string `json:"udid"`
	Mode     string `json:"mode"`
	Worker   struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	} `json:"worker"`
	WorkerID string `json:"workerId"`
}

func (r *jobRunner) buildVerify(ctx context.Context, source string, spec BuildSpec) (string, ArtifactMetadata, error) {
	root := filepath.Join(r.jobDir, "source")
	if err := unzip(source, root); err != nil {
		return "", ArtifactMetadata{}, fmt.Errorf("extract source: %w", err)
	}
	root = collapseSingleDirectory(root)
	return r.buildVerifyAt(ctx, root, spec, filepath.Join(r.jobDir, "DerivedData"))
}

func (r *jobRunner) buildVerifyAt(ctx context.Context, root string, spec BuildSpec, derivedData string) (string, ArtifactMetadata, error) {
	app, err := r.buildSimulatorAppAt(ctx, root, spec, derivedData)
	if err != nil {
		return "", ArtifactMetadata{}, err
	}
	r.strategy = "verify"
	metadata, err := readAppBundleMetadata(app)
	if err != nil {
		return "", ArtifactMetadata{}, fmt.Errorf("read app metadata: %w", err)
	}
	if spec.BundleID != "" {
		metadata.BundleID = spec.BundleID
	}
	if metadata.BundleID == "" {
		return "", metadata, errors.New("verify could not determine the app bundle id")
	}
	evidence, err := r.runVerification(ctx, root, app, metadata, spec)
	if err != nil {
		return evidence, metadata, &verifyFailureError{err: err, evidence: evidence, metadata: metadata}
	}
	return evidence, metadata, nil
}

func (r *jobRunner) runVerification(ctx context.Context, root, app string, metadata ArtifactMetadata, spec BuildSpec) (string, error) {
	cfg := r.agent.cfg
	if cfg.AgentDeviceURL == "" || cfg.AgentDeviceToken == "" {
		return "", errors.New("verify requires SIGNCLOUD_AGENT_DEVICE_URL and SIGNCLOUD_AGENT_DEVICE_TOKEN")
	}
	timeout := defaultVerifyTimeout
	if spec.VerifyTimeoutSecs > 0 {
		timeout = time.Duration(spec.VerifyTimeoutSecs) * time.Second
	}
	verifyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	device := strings.TrimSpace(spec.Device)
	if device == "" {
		device = defaultVerifyDevice
	}
	session := r.buildID
	evidenceDir := filepath.Join(r.jobDir, "evidence")
	if err := os.MkdirAll(evidenceDir, 0o750); err != nil {
		return "", fmt.Errorf("prepare evidence directory: %w", err)
	}
	commandLog, err := os.Create(filepath.Join(evidenceDir, "commands.log"))
	if err != nil {
		return "", fmt.Errorf("prepare evidence log: %w", err)
	}
	defer commandLog.Close()

	started := time.Now()
	metrics := &VerificationMetrics{
		Device:  device,
		OS:      spec.DeviceOS,
		Mode:    spec.DeviceMode,
		Session: session,
		Script:  spec.VerifyScript,
		Steps:   []string{},
	}
	r.verification = metrics
	fail := func(cause error) (string, error) {
		metrics.Verdict = "failed"
		metrics.DurationMS = time.Since(started).Milliseconds()
		// Evidence collection outlives verifyCtx: a timed-out verification must
		// still upload the screenshot taken at the failure point.
		evidenceCtx, evidenceCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer evidenceCancel()
		r.captureEvidence(evidenceCtx, session, evidenceDir, "failure")
		evidence, packErr := r.packageEvidence(evidenceCtx, evidenceDir, metadata, metrics, cause)
		if packErr != nil {
			return "", fmt.Errorf("%w (evidence packaging failed: %v)", cause, packErr)
		}
		return evidence, cause
	}

	// The session must be released even when the job times out; a stale lease
	// would keep the device busy for every later verification.
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer closeCancel()
		_, _ = r.agentDeviceOutput(closeCtx, session, "close")
	}()

	runStep := func(label string, args ...string) ([]byte, error) {
		metrics.Steps = append(metrics.Steps, label)
		fmt.Fprintf(commandLog, "== %s ==\n", label)
		output, err := r.agentDeviceOutput(verifyCtx, session, args...)
		if len(output) > 0 {
			commandLog.Write(output)
			if !bytes.HasSuffix(output, []byte("\n")) {
				commandLog.WriteString("\n")
			}
		}
		return output, err
	}
	stepError := func(label string, err error, output []byte) error {
		metrics.FailedStep = label
		tail := tailText(string(output), verifyStepTailBytes)
		if tail != "" {
			return fmt.Errorf("%s failed: %w: %s", label, err, tail)
		}
		return fmt.Errorf("%s failed: %w", label, err)
	}

	openArgs := []string{"open", "--platform", "ios", "--device", device}
	if spec.DeviceOS != "" {
		openArgs = append(openArgs, "--os", spec.DeviceOS)
	}
	if output, err := runStep("lease", "connect", "proxy", "--daemon-base-url", cfg.AgentDeviceURL); err != nil {
		return fail(stepError("connect", err, output))
	}
	if output, err := runStep("open", append(openArgs, "--timeout", "180000")...); err != nil {
		return fail(stepError("lease device", err, output))
	}

	lease, leaseErr := r.discoverLease(verifyCtx, session)
	if leaseErr != nil {
		r.agent.log(ctx, r.buildID, "warning: could not identify the simlock lease: "+leaseErr.Error()+"\n")
	} else {
		metrics.LeaseID = lease.ID
		metrics.Device = lease.Device
		metrics.OS = lease.OS
		metrics.Mode = lease.Mode
		r.agent.log(ctx, r.buildID, fmt.Sprintf("lease %s: %s %s (%s)\n", lease.ID, lease.Device, lease.OS, lease.Mode))
		if spec.DeviceMode == "full" && lease.Mode != "full" {
			r.agent.log(ctx, r.buildID, "warning: device_mode=full requested but the worker leased a slim device\n")
		}
		if err := r.agent.reportLive(ctx, r.buildID, map[string]any{
			"lease_id":  lease.ID,
			"worker_id": lease.WorkerID,
			"device":    lease.Device,
			"os":        lease.OS,
			"mode":      lease.Mode,
			"session":   session,
		}); err != nil {
			r.agent.log(ctx, r.buildID, "warning: live view unavailable: "+err.Error()+"\n")
		}
		r.agent.stage(ctx, r.buildID, "installing")
	}

	if output, err := runStep("install", "install", app); err != nil {
		return fail(stepError("install", err, output))
	}
	if output, err := runStep("launch", "open", metadata.BundleID, "--timeout", "120000"); err != nil {
		return fail(stepError("launch", err, output))
	}

	if spec.RecordVideo {
		if _, err := runStep("record_start", "record", "start", filepath.Join(evidenceDir, "recording.mp4")); err != nil {
			r.agent.log(ctx, r.buildID, "warning: screen recording unavailable: "+err.Error()+"\n")
		}
	}

	r.agent.stage(ctx, r.buildID, "verifying")
	if spec.VerifyScript != "" {
		scriptPath, err := verifyScriptPath(root, spec.VerifyScript)
		if err != nil {
			return fail(err)
		}
		args := []string{"replay", scriptPath, "--platform", "ios", "--timeout", fmt.Sprintf("%d", timeout.Milliseconds())}
		if strings.EqualFold(filepath.Ext(scriptPath), ".yaml") || strings.EqualFold(filepath.Ext(scriptPath), ".yml") {
			args = append(args, "--maestro")
		}
		if output, err := runStep("replay", args...); err != nil {
			return fail(stepError("verify script "+spec.VerifyScript, err, output))
		}
	} else {
		select {
		case <-verifyCtx.Done():
			return fail(verifyCtx.Err())
		case <-time.After(2 * time.Second):
		}
		if output, err := runStep("snapshot", "snapshot", "-i"); err != nil {
			return fail(stepError("snapshot", err, output))
		} else if err := os.WriteFile(filepath.Join(evidenceDir, "snapshot.txt"), output, 0o640); err != nil {
			r.agent.log(ctx, r.buildID, "warning: write snapshot evidence: "+err.Error()+"\n")
		}
	}

	if spec.RecordVideo {
		if _, err := runStep("record_stop", "record", "stop"); err != nil {
			r.agent.log(ctx, r.buildID, "warning: screen recording stop failed: "+err.Error()+"\n")
		}
	}
	r.captureEvidence(verifyCtx, session, evidenceDir, "final")

	metrics.Verdict = "passed"
	metrics.DurationMS = time.Since(started).Milliseconds()
	return r.packageEvidence(verifyCtx, evidenceDir, metadata, metrics, nil)
}

func (r *jobRunner) captureEvidence(ctx context.Context, session, evidenceDir, name string) {
	path := filepath.Join(evidenceDir, name+".png")
	if _, err := r.agentDeviceOutput(ctx, session, "screenshot", path); err != nil {
		r.agent.log(ctx, r.buildID, "warning: screenshot failed: "+err.Error()+"\n")
	}
}

func (r *jobRunner) packageEvidence(ctx context.Context, evidenceDir string, metadata ArtifactMetadata, metrics *VerificationMetrics, verifyErr error) (string, error) {
	report := map[string]any{
		"verification": metrics,
		"bundle_id":    metadata.BundleID,
		"version":      metadata.Version,
		"build_number": metadata.BuildNumber,
		"finished_at":  time.Now().UTC(),
	}
	if verifyErr != nil {
		report["error"] = r.agent.redact(verifyErr.Error())
	}
	data, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(filepath.Join(evidenceDir, "report.json"), data, 0o640); err != nil {
		return "", fmt.Errorf("write verification report: %w", err)
	}
	name := strings.ReplaceAll(metadata.BundleID, ".", "-") + "-verify.zip"
	output := filepath.Join(r.jobDir, name)
	started := time.Now()
	if err := r.command(ctx, r.jobDir, nil, "/usr/bin/ditto", "-c", "-k", "--norsrc", "--noextattr", evidenceDir, output); err != nil {
		return "", fmt.Errorf("package verification evidence: %w", err)
	}
	r.packagingMS += time.Since(started).Milliseconds()
	if verifyErr != nil {
		// The evidence archive is the deliverable of a failed verification too, so
		// the caller uploads it while still reporting status=failed.
		return output, nil
	}
	return output, nil
}

func verifyScriptPath(root, script string) (string, error) {
	path := filepath.Join(root, filepath.FromSlash(script))
	cleanRoot := filepath.Clean(root) + string(os.PathSeparator)
	if !strings.HasPrefix(filepath.Clean(path)+string(os.PathSeparator), cleanRoot) {
		return "", fmt.Errorf("verify script %q escapes the source directory", script)
	}
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("verify script %q is missing: %w", script, err)
	}
	return path, nil
}

func (r *jobRunner) agentDeviceOutput(ctx context.Context, session string, args ...string) ([]byte, error) {
	cfg := r.agent.cfg
	env := []string{
		"AGENT_DEVICE_DAEMON_AUTH_TOKEN=" + cfg.AgentDeviceToken,
		"AGENT_DEVICE_SESSION=" + session,
	}
	full := append([]string{}, args...)
	full = append(full, "--session", session)
	return r.commandOutput(ctx, r.jobDir, env, cfg.AgentDeviceBin, full...)
}

func (r *jobRunner) discoverLease(ctx context.Context, session string) (leaseDetail, error) {
	output, err := r.agentDeviceOutput(ctx, session, "session", "list")
	if err != nil {
		return leaseDetail{}, err
	}
	var listing struct {
		Sessions []struct {
			Name       string `json:"name"`
			DeviceUDID string `json:"device_udid"`
			ID         string `json:"id"`
		} `json:"sessions"`
	}
	trimmed := bytes.TrimSpace(output)
	if index := bytes.IndexByte(trimmed, '{'); index > 0 {
		trimmed = trimmed[index:]
	}
	if err := json.Unmarshal(trimmed, &listing); err != nil {
		return leaseDetail{}, fmt.Errorf("decode session list: %w", err)
	}
	udid := ""
	for _, entry := range listing.Sessions {
		if entry.Name == session || strings.HasSuffix(entry.Name, ":"+session) {
			udid = entry.DeviceUDID
			if udid == "" {
				udid = entry.ID
			}
			break
		}
	}
	if udid == "" {
		return leaseDetail{}, errors.New("session list did not include the leased device")
	}
	leases, err := r.simlockLeases(ctx)
	if err != nil {
		return leaseDetail{}, err
	}
	for _, id := range leases {
		detail, err := r.simlockLease(ctx, id)
		if err != nil {
			continue
		}
		if detail.UDID == udid {
			return detail, nil
		}
	}
	return leaseDetail{}, errors.New("no simlock lease matched the agent-device session")
}

func (r *jobRunner) simlockLeases(ctx context.Context) ([]string, error) {
	var body struct {
		Leases []struct {
			ID string `json:"id"`
		} `json:"leases"`
	}
	if err := r.simlockRequest(ctx, "/v1/leases", &body); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(body.Leases))
	for _, lease := range body.Leases {
		ids = append(ids, lease.ID)
	}
	return ids, nil
}

func (r *jobRunner) simlockLease(ctx context.Context, id string) (leaseDetail, error) {
	var body struct {
		Lease leaseDetail `json:"lease"`
	}
	if err := r.simlockRequest(ctx, "/v1/leases/"+id, &body); err != nil {
		return leaseDetail{}, err
	}
	if body.Lease.WorkerID == "" {
		body.Lease.WorkerID = body.Lease.Worker.ID
	}
	return body.Lease, nil
}

func (r *jobRunner) simlockRequest(ctx context.Context, path string, target any) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, r.agent.cfg.SimlockGatewayURL+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+r.agent.cfg.AgentDeviceToken)
	response, err := (&http.Client{}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return fmt.Errorf("simlock %s returned %s: %s", path, response.Status, strings.TrimSpace(string(data)))
	}
	return json.NewDecoder(response.Body).Decode(target)
}

func (a *Agent) reportLive(ctx context.Context, id string, live map[string]any) error {
	body, err := json.Marshal(live)
	if err != nil {
		return err
	}
	requestCtx, cancel := context.WithTimeout(ctx, a.requestTimeout())
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, a.cfg.ServerURL+"/v1/agent/builds/"+id+"/live", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+a.cfg.AgentToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := a.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return fmt.Errorf("live report returned %s: %s", response.Status, strings.TrimSpace(string(data)))
	}
	return nil
}

func tailText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[len(value)-limit:]
}
