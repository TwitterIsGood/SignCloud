package control

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
	ID                   string          `json:"id"`
	Name                 string          `json:"name"`
	Kind                 string          `json:"kind"`
	Status               string          `json:"status"`
	SourceName           string          `json:"source_name"`
	ArtifactName         string          `json:"artifact_name,omitempty"`
	InstallToken         string          `json:"install_token"`
	Spec                 json.RawMessage `json:"spec"`
	Logs                 string          `json:"logs,omitempty"`
	Error                string          `json:"error,omitempty"`
	ClaimedBy            string          `json:"claimed_by,omitempty"`
	BundleID             string          `json:"bundle_id,omitempty"`
	Version              string          `json:"version,omitempty"`
	MinimumOS            string          `json:"minimum_os,omitempty"`
	MinimumSystemVersion string          `json:"minimum_system_version,omitempty"`
	BuildNumber          string          `json:"build_number,omitempty"`
	Stage                string          `json:"stage,omitempty"`
	Architectures        []string        `json:"architectures,omitempty"`
	Signing              string          `json:"signing,omitempty"`
	Signed               bool            `json:"signed"`
	Notarized            bool            `json:"notarized"`
	Stapled              bool            `json:"stapled"`
	SHA256               string          `json:"sha256,omitempty"`
	AppZipURL            string          `json:"app_zip_url,omitempty"`
	CreatedAt            time.Time       `json:"created_at"`
	UpdatedAt            time.Time       `json:"updated_at"`
	SourceURL            string          `json:"source_url,omitempty"`
	InstallURL           string          `json:"install_url,omitempty"`
	QRURL                string          `json:"qr_url,omitempty"`
	ManifestURL          string          `json:"manifest_url,omitempty"`
	OTAURL               string          `json:"ota_url,omitempty"`
	ArtifactURL          string          `json:"artifact_url,omitempty"`
	CacheKey             string          `json:"cache_key,omitempty"`
	SourceMode           string          `json:"source_mode,omitempty"`
	Metrics              json.RawMessage `json:"metrics,omitempty"`
	Live                 json.RawMessage `json:"live,omitempty"`
	Verdict              string          `json:"verdict,omitempty"`
	ViewURL              string          `json:"view_url,omitempty"`
}

type Device struct {
	ID            string    `json:"id"`
	UDID          string    `json:"udid,omitempty"`
	MaskedUDID    string    `json:"masked_udid,omitempty"`
	Name          string    `json:"name"`
	Platform      string    `json:"platform"`
	Status        string    `json:"status"`
	AppleDeviceID string    `json:"apple_device_id,omitempty"`
	Error         string    `json:"error,omitempty"`
	ClaimedBy     string    `json:"claimed_by,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	RequestedBy   string    `json:"-"`
}

func validKind(kind string) bool {
	return kind == "xcode" || kind == "expo" || kind == "resign" || kind == "simulator" || kind == "macos" || kind == "resolve" || kind == "verify"
}

func validStatus(status string) bool {
	return status == "queued" || status == "running" || status == "succeeded" || status == "failed" || status == "canceled"
}
