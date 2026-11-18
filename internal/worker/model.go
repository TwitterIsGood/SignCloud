package worker

import (
	"encoding/json"
	"time"
)

type BuildSpec struct {
	Scheme                   string            `json:"scheme,omitempty"`
	Configuration            string            `json:"configuration,omitempty"`
	TeamID                   string            `json:"team_id,omitempty"`
	BundleID                 string            `json:"bundle_id,omitempty"`
	ExportMethod             string            `json:"export_method,omitempty"`
	Workspace                string            `json:"workspace,omitempty"`
	Project                  string            `json:"project,omitempty"`
	Environment              map[string]string `json:"environment,omitempty"`
	AllowProvisioningUpdates bool              `json:"allow_provisioning_updates,omitempty"`
	Clean                    bool              `json:"clean,omitempty"`
	FastPackage              bool              `json:"fast_package,omitempty"`
	Architectures            []string          `json:"architectures,omitempty"`
	Signing                  string            `json:"signing,omitempty"`
	Notarize                 bool              `json:"notarize,omitempty"`
	ArtifactFormat           string            `json:"artifact_format,omitempty"`
	IncludeAppZip            bool              `json:"include_app_zip,omitempty"`
	HardenedRuntime          *bool             `json:"hardened_runtime,omitempty"`
	XcodeVersion             string            `json:"xcode_version,omitempty"`
	Device                   string            `json:"device,omitempty"`
	DeviceOS                 string            `json:"device_os,omitempty"`
	DeviceMode               string            `json:"device_mode,omitempty"`
	VerifyScript             string            `json:"verify_script,omitempty"`
	RecordVideo              bool              `json:"record_video,omitempty"`
	VerifyTimeoutSecs        int               `json:"verify_timeout_seconds,omitempty"`
}

type Build struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Kind       string          `json:"kind"`
	Status     string          `json:"status"`
	SourceName string          `json:"source_name"`
	SourceURL  string          `json:"source_url"`
	SourceMode string          `json:"source_mode"`
	CacheKey   string          `json:"cache_key"`
	Metrics    json.RawMessage `json:"metrics"`
	Spec       json.RawMessage `json:"spec"`
	CreatedAt  time.Time       `json:"created_at"`
}

type Device struct {
	ID       string `json:"id"`
	UDID     string `json:"udid"`
	Name     string `json:"name"`
	Platform string `json:"platform"`
	Status   string `json:"status"`
}

type ManifestFile struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
	Size int64  `json:"size"`
	Mode uint32 `json:"mode,omitempty"`
	// Link marks a symbolic link; it carries the relative target instead of a blob.
	Link string `json:"link,omitempty"`
}

type ProjectManifest struct {
	Version  int            `json:"version"`
	CacheKey string         `json:"cache_key"`
	Files    []ManifestFile `json:"files"`
}

type SyncMetrics struct {
	FilesTotal                int                  `json:"files_total"`
	FilesChanged              int                  `json:"files_changed"`
	FilesReused               int                  `json:"files_reused"`
	FilesRemoved              int                  `json:"files_removed"`
	BlobBytesFetched          int64                `json:"blob_bytes_fetched"`
	SourceSyncMS              int64                `json:"source_sync_ms"`
	BuildMS                   int64                `json:"build_ms"`
	XcodeMS                   int64                `json:"xcode_ms,omitempty"`
	ArchiveMS                 int64                `json:"archive_ms,omitempty"`
	ExportMS                  int64                `json:"export_ms,omitempty"`
	DependencyMS              int64                `json:"dependency_ms,omitempty"`
	PrebuildMS                int64                `json:"prebuild_ms,omitempty"`
	PackagingMS               int64                `json:"packaging_ms,omitempty"`
	CPUUserMS                 int64                `json:"cpu_user_ms,omitempty"`
	CPUSystemMS               int64                `json:"cpu_system_ms,omitempty"`
	PeakRSSBytes              int64                `json:"peak_rss_bytes,omitempty"`
	CacheHit                  bool                 `json:"cache_hit"`
	ResultCacheHit            bool                 `json:"result_cache_hit,omitempty"`
	DependencyCacheHit        bool                 `json:"dependency_cache_hit,omitempty"`
	PodCacheHit               bool                 `json:"pod_cache_hit,omitempty"`
	ResultCacheDisabledReason string               `json:"result_cache_disabled_reason,omitempty"`
	BuildStrategy             string               `json:"build_strategy,omitempty"`
	FallbackReason            string               `json:"fallback_reason,omitempty"`
	Verification              *VerificationMetrics `json:"verification,omitempty"`
}

type VerificationMetrics struct {
	Verdict    string   `json:"verdict"`
	Device     string   `json:"device,omitempty"`
	OS         string   `json:"os,omitempty"`
	Mode       string   `json:"mode,omitempty"`
	Session    string   `json:"session,omitempty"`
	Script     string   `json:"script,omitempty"`
	LeaseID    string   `json:"lease_id,omitempty"`
	DurationMS int64    `json:"duration_ms"`
	FailedStep string   `json:"failed_step,omitempty"`
	Steps      []string `json:"steps,omitempty"`
}

type ArtifactMetadata struct {
	BundleID      string
	Version       string
	BuildNumber   string
	MinimumOS     string
	Architectures []string
	Signing       string
	Signed        bool
	Notarized     bool
	Stapled       bool
	SHA256        string
	AppZip        string
}
