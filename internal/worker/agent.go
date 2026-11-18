package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Agent struct {
	cfg           Config
	client        *http.Client
	buildHomeOnce sync.Once
	buildHome     string
	buildHomeErr  error
	cleanupMu     sync.Mutex
	lastCleanup   time.Time
	toolchainMu   sync.Mutex
	toolchains    map[string]string
	xcodesMu      sync.Mutex
	xcodes        []XcodeInstall
	xcodesErr     error
	xcodesLoaded  bool
}

const (
	defaultRequestTimeout = 30 * time.Second
	uploadTimeout         = 30 * time.Minute
)

func New(cfg Config) *Agent {
	return &Agent{cfg: cfg, client: &http.Client{}}
}

// requestTimeout keeps a stalled control-plane connection from wedging the
// agent loop forever. Artifact uploads use uploadTimeout instead, since they
// legitimately carry large bodies.
func (a *Agent) requestTimeout() time.Duration {
	if a.cfg.RequestTimeout > 0 {
		return a.cfg.RequestTimeout
	}
	return defaultRequestTimeout
}

func (a *Agent) Run(ctx context.Context, once bool) error {
	if err := os.MkdirAll(a.cfg.WorkRoot, 0o750); err != nil {
		return err
	}
	log.Printf("SignCloud agent %q connected to %s", a.cfg.WorkerID, a.cfg.ServerURL)
	for {
		device, deviceErr := a.claimDevice(ctx)
		if deviceErr != nil {
			if errors.Is(deviceErr, context.Canceled) {
				return nil
			}
			if once {
				return deviceErr
			}
			log.Printf("claim device failed: %v", deviceErr)
		} else if device != nil {
			a.processDevice(ctx, *device)
			if once {
				return nil
			}
			continue
		}
		build, err := a.claim(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			if once {
				return err
			}
			log.Printf("claim failed: %v", err)
		} else if build != nil {
			a.process(ctx, *build)
			if once {
				return nil
			}
		} else if once {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(a.cfg.PollInterval):
		}
	}
}

func (a *Agent) claim(ctx context.Context) (*Build, error) {
	ctx, cancel := context.WithTimeout(ctx, a.requestTimeout())
	defer cancel()
	body, _ := json.Marshal(map[string]string{"worker_id": a.cfg.WorkerID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.ServerURL+"/v1/agent/claim", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.AgentToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("claim returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var build Build
	if err := json.NewDecoder(resp.Body).Decode(&build); err != nil {
		return nil, err
	}
	return &build, nil
}

func (a *Agent) process(ctx context.Context, build Build) {
	parent := ctx
	ctx, cancelBuild := context.WithCancel(ctx)
	defer cancelBuild()
	go a.watchBuildCancellation(ctx, cancelBuild, build.ID)

	jobDir := filepath.Join(a.cfg.WorkRoot, build.ID)
	_ = os.RemoveAll(jobDir)
	if err := os.MkdirAll(jobDir, 0o750); err != nil {
		a.fail(ctx, build.ID, err)
		return
	}
	defer os.RemoveAll(jobDir)
	if build.Kind != "macos" {
		a.stage(ctx, build.ID, "preparing")
	}
	if a.cfg.EphemeralKeychain && build.Kind != "simulator" && build.Kind != "macos" && build.Kind != "resolve" && build.Kind != "verify" {
		signingSession, err := a.beginEphemeralKeychain(jobDir)
		if err != nil {
			a.fail(ctx, build.ID, fmt.Errorf("prepare ephemeral signing keychain: %w", err))
			return
		}
		defer func() {
			if err := signingSession.Close(); err != nil {
				log.Printf("build %s ephemeral keychain cleanup failed: %v", build.ID, err)
			}
		}()
		a.log(ctx, build.ID, "Ephemeral signing keychain ready; it will be deleted after this build.\n")
	}

	a.log(ctx, build.ID, fmt.Sprintf("[%s] worker=%s kind=%s source=%s\n", time.Now().Format(time.RFC3339), a.cfg.WorkerID, build.Kind, build.SourceName))
	sourcePath := filepath.Join(jobDir, filepath.Base(build.SourceName))
	if err := a.download(ctx, build.SourceURL, sourcePath); err != nil {
		a.fail(ctx, build.ID, fmt.Errorf("download source: %w", err))
		return
	}
	var spec BuildSpec
	if len(build.Spec) > 0 {
		if err := json.Unmarshal(build.Spec, &spec); err != nil {
			a.fail(ctx, build.ID, fmt.Errorf("decode build spec: %w", err))
			return
		}
	}

	developerDir, xcodeErr := a.xcodeDeveloperDir(spec.XcodeVersion)
	if xcodeErr != nil {
		a.fail(ctx, build.ID, xcodeErr)
		return
	}
	if version := a.xcodeVersionForDeveloperDir(developerDir); version != "" {
		a.log(ctx, build.ID, "Using Xcode "+version+" ("+developerDir+")\n")
	}

	runner := &jobRunner{agent: a, buildID: build.ID, buildName: build.Name, jobDir: jobDir, developerDir: developerDir}
	var artifact string
	var metadata ArtifactMetadata
	metadataReady := false
	resultFingerprint := ""
	var workspace *cacheWorkspace
	var err error
	metrics := SyncMetrics{}
	buildStarted := time.Now()
	if build.SourceMode == "cas" {
		var syncMetrics SyncMetrics
		var materializeErr error
		workspace, syncMetrics, materializeErr = a.materializeCAS(ctx, build, sourcePath, developerDir)
		metrics = syncMetrics
		if materializeErr != nil {
			a.fail(ctx, build.ID, materializeErr)
			return
		}
		defer func() {
			workspace.close()
			a.cleanupCaches(workspace.root)
		}()
		if spec.Clean {
			if err := os.RemoveAll(workspace.DerivedData); err != nil {
				a.fail(ctx, build.ID, fmt.Errorf("clean DerivedData: %w", err))
				return
			}
			if err := os.MkdirAll(workspace.DerivedData, 0o750); err != nil {
				a.fail(ctx, build.ID, err)
				return
			}
			metrics.CacheHit = false
		}
		if !spec.Clean {
			if eligible, reason := resultCacheEligible(workspace, build.Kind); eligible {
				fingerprint, fingerprintErr := a.resultFingerprint(workspace, build, spec, developerDir)
				if fingerprintErr != nil {
					a.log(ctx, build.ID, "warning: result cache disabled: "+fingerprintErr.Error()+"\n")
				} else if cachedArtifact, cachedMetadata, hit := cachedResult(workspace, fingerprint); hit {
					artifact = cachedArtifact
					metadata = cachedMetadata
					metadataReady = true
					metrics.ResultCacheHit = true
					runner.strategy = "result-cache"
					a.log(ctx, build.ID, "result cache hit: Xcode execution skipped\n")
				} else {
					resultFingerprint = fingerprint
				}
			} else {
				metrics.ResultCacheDisabledReason = reason
				a.log(ctx, build.ID, "result cache disabled: "+reason+"\n")
			}
		}
		if artifact == "" {
			a.startBuildStage(ctx, build)
			switch build.Kind {
			case "xcode":
				artifact, err = runner.buildXcodeAt(ctx, workspace.Source, spec, workspace.DerivedData)
			case "simulator":
				artifact, err = runner.buildSimulatorAt(ctx, workspace.Source, spec, workspace.DerivedData)
			case "expo":
				artifact, err = runner.buildExpoAt(ctx, workspace.Source, spec, workspace.DerivedData)
			case "macos":
				artifact, metadata, err = runner.buildMacOSAt(ctx, workspace.Source, spec, workspace.DerivedData)
				metadataReady = err == nil
			case "resolve":
				artifact, err = runner.resolvePackagesAt(ctx, workspace.Source, spec)
			case "verify":
				artifact, metadata, err = runner.buildVerifyAt(ctx, workspace.Source, spec, workspace.DerivedData)
				metadataReady = metadata.BundleID != ""
			default:
				err = fmt.Errorf("unsupported incremental build kind %q", build.Kind)
			}
		}
	} else {
		a.startBuildStage(ctx, build)
		switch build.Kind {
		case "xcode":
			artifact, err = runner.buildXcode(ctx, sourcePath, spec)
		case "simulator":
			artifact, err = runner.buildSimulator(ctx, sourcePath, spec)
		case "expo":
			artifact, err = runner.buildExpo(ctx, sourcePath, spec)
		case "resign":
			artifact, err = runner.resignIPA(ctx, sourcePath)
		case "macos":
			artifact, metadata, err = runner.buildMacOS(ctx, sourcePath, spec)
			metadataReady = err == nil
		case "resolve":
			artifact, err = runner.resolvePackages(ctx, sourcePath, spec)
		case "verify":
			artifact, metadata, err = runner.buildVerify(ctx, sourcePath, spec)
			metadataReady = metadata.BundleID != ""
		default:
			err = fmt.Errorf("unsupported build kind %q", build.Kind)
		}
	}
	metrics.BuildMS = time.Since(buildStarted).Milliseconds()
	runner.applyMetrics(&metrics)
	if err != nil {
		if a.controlSaysCanceled(parent, build.ID) {
			log.Printf("build %s canceled by client; discarding partial result", build.ID)
			return
		}
		var verifyErr *verifyFailureError
		if errors.As(err, &verifyErr) && verifyErr.evidence != "" {
			a.failWithEvidence(ctx, build.ID, err, verifyErr.evidence, verifyErr.metadata, metrics)
			return
		}
		a.failWithMetrics(ctx, build.ID, err, metrics)
		return
	}
	if build.Kind != "macos" {
		a.stage(ctx, build.ID, "packaging")
	}
	if !metadataReady && build.Kind != "resolve" {
		metadata, err = readArtifactMetadata(artifact, jobDir)
		if err != nil {
			a.log(ctx, build.ID, "warning: could not read artifact metadata: "+err.Error()+"\n")
		}
	}
	if workspace != nil && resultFingerprint != "" && !metrics.ResultCacheHit {
		if cacheErr := storeCachedResult(workspace, resultFingerprint, artifact, metadata); cacheErr != nil {
			a.log(ctx, build.ID, "warning: store result cache: "+cacheErr.Error()+"\n")
		}
	}
	a.stage(ctx, build.ID, "uploading")
	if err := a.complete(ctx, build.ID, artifact, metadata, metrics); err != nil {
		log.Printf("build %s completed locally but upload failed: %v", build.ID, err)
		return
	}
	log.Printf("build %s succeeded", build.ID)
}

func (a *Agent) download(ctx context.Context, url, path string) error {
	ctx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()
	if strings.HasPrefix(url, "/") {
		url = a.cfg.ServerURL + url
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.AgentToken)
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned %s", resp.Status)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(file, resp.Body)
	return err
}

// watchBuildCancellation polls the control plane while a build runs and cancels
// the build context as soon as the client cancels the task, which aborts the
// running xcodebuild/notarytool process instead of letting it finish unused.
func (a *Agent) watchBuildCancellation(ctx context.Context, cancel context.CancelFunc, id string) {
	ticker := time.NewTicker(a.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		canceled, err := a.buildCanceled(ctx, id)
		if err != nil {
			continue
		}
		if canceled {
			log.Printf("build %s canceled by client; stopping worker", id)
			cancel()
			return
		}
	}
}

func (a *Agent) buildCanceled(ctx context.Context, id string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, a.requestTimeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.ServerURL+"/v1/agent/builds/"+id+"/status", nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.AgentToken)
	resp, err := a.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("status returned %s", resp.Status)
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false, err
	}
	return body.Status == "canceled", nil
}

func (a *Agent) controlSaysCanceled(ctx context.Context, id string) bool {
	canceled, err := a.buildCanceled(ctx, id)
	return err == nil && canceled
}

func (a *Agent) log(ctx context.Context, id, message string) {
	message = a.redact(message)
	log.Print(strings.TrimSuffix(message, "\n"))
	ctx, cancel := context.WithTimeout(ctx, a.requestTimeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.ServerURL+"/v1/agent/builds/"+id+"/logs", strings.NewReader(message))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.AgentToken)
	resp, err := a.client.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

func (a *Agent) startBuildStage(ctx context.Context, build Build) {
	if build.Kind == "macos" {
		return
	}
	if build.Kind == "resign" {
		a.stage(ctx, build.ID, "signing")
		return
	}
	if build.Kind == "resolve" {
		a.stage(ctx, build.ID, "resolving")
		return
	}
	a.stage(ctx, build.ID, "building")
}

func (a *Agent) stage(ctx context.Context, id, stage string) {
	body, _ := json.Marshal(map[string]string{"stage": stage})
	reqCtx, cancel := context.WithTimeout(ctx, a.requestTimeout())
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, a.cfg.ServerURL+"/v1/agent/builds/"+id+"/stage", bytes.NewReader(body))
	if err == nil {
		req.Header.Set("Authorization", "Bearer "+a.cfg.AgentToken)
		req.Header.Set("Content-Type", "application/json")
		if resp, requestErr := a.client.Do(req); requestErr == nil {
			resp.Body.Close()
		}
	}
	cancel()
	a.log(ctx, id, "stage: "+stage+"\n")
}

func (a *Agent) fail(ctx context.Context, id string, buildErr error) {
	a.failWithMetrics(ctx, id, buildErr, SyncMetrics{})
}

func (a *Agent) failWithMetrics(ctx context.Context, id string, buildErr error, metrics SyncMetrics) {
	safeError := a.redact(buildErr.Error())
	a.log(ctx, id, "ERROR: "+safeError+"\n")
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	_ = writer.WriteField("status", "failed")
	_ = writer.WriteField("error", safeError)
	metricsJSON, _ := json.Marshal(metrics)
	_ = writer.WriteField("metrics", string(metricsJSON))
	_ = writer.Close()
	if err := a.sendComplete(ctx, id, body, writer.FormDataContentType()); err != nil {
		log.Printf("report failure for %s: %v", id, err)
	}
}

func (a *Agent) complete(ctx context.Context, id, artifact string, metadata ArtifactMetadata, metrics SyncMetrics) error {
	return a.uploadResult(ctx, id, map[string]string{"status": "succeeded"}, artifact, metadata, metrics)
}

// failWithEvidence reports a failed verification while still uploading the
// evidence archive, so the failing run stays inspectable.
func (a *Agent) failWithEvidence(ctx context.Context, id string, buildErr error, artifact string, metadata ArtifactMetadata, metrics SyncMetrics) {
	safeError := a.redact(buildErr.Error())
	a.log(ctx, id, "ERROR: "+safeError+"\n")
	fields := map[string]string{"status": "failed", "error": safeError}
	if err := a.uploadResult(ctx, id, fields, artifact, metadata, metrics); err != nil {
		log.Printf("report failure with evidence for %s: %v", id, err)
	}
}

func (a *Agent) uploadResult(ctx context.Context, id string, extraFields map[string]string, artifact string, metadata ArtifactMetadata, metrics SyncMetrics) error {
	reader, writerPipe := io.Pipe()
	writer := multipart.NewWriter(writerPipe)
	contentType := writer.FormDataContentType()
	writeResult := make(chan error, 1)
	go func() {
		var result error
		defer func() {
			if closeErr := writer.Close(); result == nil {
				result = closeErr
			}
			_ = writerPipe.CloseWithError(result)
			writeResult <- result
		}()
		fields := map[string]string{
			"bundle_id":    metadata.BundleID,
			"version":      metadata.Version,
			"minimum_os":   metadata.MinimumOS,
			"build_number": metadata.BuildNumber,
			"signing":      metadata.Signing,
			"signed":       fmt.Sprintf("%t", metadata.Signed),
			"notarized":    fmt.Sprintf("%t", metadata.Notarized),
			"stapled":      fmt.Sprintf("%t", metadata.Stapled),
			"sha256":       metadata.SHA256,
		}
		architectures, _ := json.Marshal(metadata.Architectures)
		fields["architectures"] = string(architectures)
		metricsJSON, _ := json.Marshal(metrics)
		fields["metrics"] = string(metricsJSON)
		for key, value := range extraFields {
			fields[key] = value
		}
		for key, value := range fields {
			if result = writer.WriteField(key, value); result != nil {
				return
			}
		}
		if artifact != "" {
			header := make(textproto.MIMEHeader)
			header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="artifact"; filename=%q`, filepath.Base(artifact)))
			header.Set("Content-Type", "application/octet-stream")
			part, err := writer.CreatePart(header)
			if err != nil {
				result = err
				return
			}
			file, err := os.Open(artifact)
			if err != nil {
				result = err
				return
			}
			_, result = io.Copy(part, file)
			if closeErr := file.Close(); result == nil {
				result = closeErr
			}
		}
		if result == nil && metadata.AppZip != "" {
			zipHeader := make(textproto.MIMEHeader)
			zipHeader.Set("Content-Disposition", fmt.Sprintf(`form-data; name="app_zip"; filename=%q`, filepath.Base(metadata.AppZip)))
			zipHeader.Set("Content-Type", "application/zip")
			zipPart, zipErr := writer.CreatePart(zipHeader)
			if zipErr != nil {
				result = zipErr
				return
			}
			appZip, openErr := os.Open(metadata.AppZip)
			if openErr != nil {
				result = openErr
				return
			}
			_, result = io.Copy(zipPart, appZip)
			if closeErr := appZip.Close(); result == nil {
				result = closeErr
			}
		}
	}()
	requestErr := a.sendComplete(ctx, id, reader, contentType)
	writeErr := <-writeResult
	if requestErr != nil {
		return requestErr
	}
	return writeErr
}

func (a *Agent) sendComplete(ctx context.Context, id string, body io.Reader, contentType string) error {
	ctx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.ServerURL+"/v1/agent/builds/"+id+"/complete", body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.AgentToken)
	req.Header.Set("Content-Type", contentType)
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("complete returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return nil
}
