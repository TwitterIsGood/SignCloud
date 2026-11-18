package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ServerURL                 string
	AgentToken                string
	WorkerID                  string
	WorkRoot                  string
	CacheRoot                 string
	CacheMaxGB                int64
	CacheTTL                  time.Duration
	CompilationCache          string
	CompilationCacheMaxGB     int64
	BlobCacheMaxGB            int64
	TeamID                    string
	SigningIdentity           string
	ProvisioningProfile       string
	WildcardProfile           string
	KeychainPath              string
	KeychainPasswordFile      string
	EphemeralKeychain         bool
	SigningP12Path            string
	SigningP12PasswordFile    string
	ExportMethod              string
	APIKeyPath                string
	APIKeyID                  string
	APIIssuerID               string
	AppleAPIBaseURL           string
	MacOSDeveloperIDIdentity  string
	MacOSKeychainPath         string
	MacOSKeychainPasswordFile string
	MacOSNotaryProfile        string
	MacOSSigningConfigVersion string
	MacOSXcodeGenEnabled      bool
	XcodePaths                []string
	AgentDeviceURL            string
	AgentDeviceToken          string
	AgentDeviceBin            string
	SimlockGatewayURL         string
	PollInterval              time.Duration
	RequestTimeout            time.Duration
}

func ConfigFromEnv() (Config, error) {
	host, _ := os.Hostname()
	cfg := Config{
		ServerURL:                 strings.TrimRight(os.Getenv("SIGNCLOUD_SERVER"), "/"),
		AgentToken:                os.Getenv("SIGNCLOUD_AGENT_TOKEN"),
		WorkerID:                  envOr("SIGNCLOUD_WORKER_ID", host),
		WorkRoot:                  envOr("SIGNCLOUD_WORK_ROOT", "./work"),
		CacheRoot:                 envOr("SIGNCLOUD_CACHE_ROOT", "./cache"),
		CacheMaxGB:                100,
		CacheTTL:                  14 * 24 * time.Hour,
		TeamID:                    os.Getenv("APPLE_TEAM_ID"),
		SigningIdentity:           envOr("IOS_SIGNING_IDENTITY", "Apple Development"),
		ProvisioningProfile:       os.Getenv("IOS_PROVISIONING_PROFILE"),
		WildcardProfile:           os.Getenv("IOS_WILDCARD_PROFILE"),
		KeychainPath:              os.Getenv("IOS_KEYCHAIN_PATH"),
		KeychainPasswordFile:      os.Getenv("IOS_KEYCHAIN_PASSWORD_FILE"),
		SigningP12Path:            os.Getenv("IOS_SIGNING_P12_PATH"),
		SigningP12PasswordFile:    os.Getenv("IOS_SIGNING_P12_PASSWORD_FILE"),
		ExportMethod:              envOr("IOS_EXPORT_METHOD", "development"),
		APIKeyPath:                os.Getenv("APPLE_API_KEY_PATH"),
		APIKeyID:                  os.Getenv("APPLE_API_KEY_ID"),
		APIIssuerID:               os.Getenv("APPLE_API_ISSUER_ID"),
		AppleAPIBaseURL:           envOr("APPLE_API_BASE_URL", "https://api.appstoreconnect.apple.com"),
		MacOSDeveloperIDIdentity:  os.Getenv("MACOS_DEVELOPER_ID_IDENTITY"),
		MacOSKeychainPath:         os.Getenv("MACOS_KEYCHAIN_PATH"),
		MacOSKeychainPasswordFile: os.Getenv("MACOS_KEYCHAIN_PASSWORD_FILE"),
		MacOSNotaryProfile:        os.Getenv("MACOS_NOTARY_KEYCHAIN_PROFILE"),
		MacOSSigningConfigVersion: envOr("MACOS_SIGNING_CONFIG_VERSION", "1"),
		XcodePaths:                xcodePathsFromEnv(os.Getenv("SIGNCLOUD_XCODE_PATHS")),
		AgentDeviceURL:            strings.TrimRight(os.Getenv("SIGNCLOUD_AGENT_DEVICE_URL"), "/"),
		AgentDeviceToken:          os.Getenv("SIGNCLOUD_AGENT_DEVICE_TOKEN"),
		AgentDeviceBin:            envOr("SIGNCLOUD_AGENT_DEVICE_BIN", "agent-device"),
		SimlockGatewayURL:         strings.TrimRight(envOr("SIGNCLOUD_SIMLOCK_GATEWAY_URL", "http://127.0.0.1:4700"), "/"),
		PollInterval:              time.Second,
		RequestTimeout:            30 * time.Second,
	}
	if raw := os.Getenv("SIGNCLOUD_POLL_SECONDS"); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds < 1 {
			return Config{}, fmt.Errorf("SIGNCLOUD_POLL_SECONDS must be a positive integer")
		}
		cfg.PollInterval = time.Duration(seconds) * time.Second
	}
	if raw := os.Getenv("SIGNCLOUD_REQUEST_TIMEOUT_SECONDS"); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds < 1 {
			return Config{}, fmt.Errorf("SIGNCLOUD_REQUEST_TIMEOUT_SECONDS must be a positive integer")
		}
		cfg.RequestTimeout = time.Duration(seconds) * time.Second
	}
	if raw := os.Getenv("SIGNCLOUD_CACHE_MAX_GB"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 1 {
			return Config{}, fmt.Errorf("SIGNCLOUD_CACHE_MAX_GB must be a positive integer")
		}
		cfg.CacheMaxGB = value
	}
	for name, target := range map[string]*int64{
		"SIGNCLOUD_COMPILATION_CACHE_MAX_GB": &cfg.CompilationCacheMaxGB,
		"SIGNCLOUD_BLOB_CACHE_MAX_GB":        &cfg.BlobCacheMaxGB,
	} {
		if raw := strings.TrimSpace(os.Getenv(name)); raw != "" {
			value, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || value < 1 {
				return Config{}, fmt.Errorf("%s must be a positive integer", name)
			}
			*target = value
		}
	}
	if raw := os.Getenv("SIGNCLOUD_CACHE_TTL_HOURS"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 1 {
			return Config{}, fmt.Errorf("SIGNCLOUD_CACHE_TTL_HOURS must be a positive integer")
		}
		cfg.CacheTTL = time.Duration(value) * time.Hour
	}
	cfg.CompilationCache = compilationCacheAuto
	if raw := strings.ToLower(strings.TrimSpace(os.Getenv("SIGNCLOUD_COMPILATION_CACHE"))); raw != "" {
		if raw != compilationCacheAuto && raw != compilationCacheOff {
			return Config{}, fmt.Errorf("SIGNCLOUD_COMPILATION_CACHE must be auto or off")
		}
		cfg.CompilationCache = raw
	}
	if raw := strings.TrimSpace(os.Getenv("IOS_EPHEMERAL_KEYCHAIN")); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("IOS_EPHEMERAL_KEYCHAIN must be a boolean")
		}
		cfg.EphemeralKeychain = value
	}
	if raw := strings.TrimSpace(os.Getenv("MACOS_XCODEGEN_ENABLED")); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("MACOS_XCODEGEN_ENABLED must be a boolean")
		}
		cfg.MacOSXcodeGenEnabled = value
	}
	if cfg.ServerURL == "" || cfg.AgentToken == "" || cfg.WorkerID == "" {
		return Config{}, fmt.Errorf("SIGNCLOUD_SERVER, SIGNCLOUD_AGENT_TOKEN and SIGNCLOUD_WORKER_ID are required")
	}
	abs, err := filepath.Abs(cfg.WorkRoot)
	if err != nil {
		return Config{}, err
	}
	cfg.WorkRoot = abs
	cfg.CacheRoot, err = filepath.Abs(cfg.CacheRoot)
	if err != nil {
		return Config{}, err
	}
	if cfg.ProvisioningProfile != "" {
		cfg.ProvisioningProfile, err = filepath.Abs(cfg.ProvisioningProfile)
		if err != nil {
			return Config{}, err
		}
	}
	cfg.AppleAPIBaseURL = strings.TrimRight(cfg.AppleAPIBaseURL, "/")
	if cfg.KeychainPath != "" {
		cfg.KeychainPath, err = filepath.Abs(cfg.KeychainPath)
		if err != nil {
			return Config{}, err
		}
	}
	if cfg.KeychainPasswordFile != "" {
		cfg.KeychainPasswordFile, err = filepath.Abs(cfg.KeychainPasswordFile)
		if err != nil {
			return Config{}, err
		}
	}
	if cfg.SigningP12Path != "" {
		cfg.SigningP12Path, err = filepath.Abs(cfg.SigningP12Path)
		if err != nil {
			return Config{}, err
		}
	}
	if cfg.SigningP12PasswordFile != "" {
		cfg.SigningP12PasswordFile, err = filepath.Abs(cfg.SigningP12PasswordFile)
		if err != nil {
			return Config{}, err
		}
	}
	if cfg.MacOSKeychainPath != "" {
		cfg.MacOSKeychainPath, err = filepath.Abs(cfg.MacOSKeychainPath)
		if err != nil {
			return Config{}, err
		}
	}
	if cfg.MacOSKeychainPasswordFile != "" {
		cfg.MacOSKeychainPasswordFile, err = filepath.Abs(cfg.MacOSKeychainPasswordFile)
		if err != nil {
			return Config{}, err
		}
	}
	if cfg.EphemeralKeychain && (cfg.SigningP12Path == "" || cfg.SigningP12PasswordFile == "") {
		return Config{}, fmt.Errorf("IOS_SIGNING_P12_PATH and IOS_SIGNING_P12_PASSWORD_FILE are required when IOS_EPHEMERAL_KEYCHAIN is enabled")
	}
	return cfg, nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
