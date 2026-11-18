package control

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr              string
	DatabaseURL             string
	StorageDir              string
	PublicBaseURL           string
	APIToken                string
	APITokens               []string
	AgentToken              string
	MaxUploadMB             int64
	MaxDevicesPerToken      int
	DeviceEnrollmentEnabled bool
	StaleBuildTimeout       time.Duration
	SimlockGatewayURL       string
	SimlockOperatorToken    string
}

func ConfigFromEnv() (Config, error) {
	cfg := Config{
		ListenAddr:              envOr("SIGNCLOUD_LISTEN_ADDR", ":8787"),
		DatabaseURL:             os.Getenv("DATABASE_URL"),
		StorageDir:              envOr("SIGNCLOUD_STORAGE_DIR", "./data"),
		PublicBaseURL:           strings.TrimRight(envOr("SIGNCLOUD_PUBLIC_BASE_URL", "http://localhost:8787"), "/"),
		APIToken:                os.Getenv("SIGNCLOUD_API_TOKEN"),
		APITokens:               tokenList(os.Getenv("SIGNCLOUD_API_TOKEN"), os.Getenv("SIGNCLOUD_API_TOKENS")),
		AgentToken:              os.Getenv("SIGNCLOUD_AGENT_TOKEN"),
		MaxUploadMB:             2048,
		MaxDevicesPerToken:      3,
		DeviceEnrollmentEnabled: false,
		StaleBuildTimeout:       6 * time.Hour,
		SimlockGatewayURL:       strings.TrimRight(envOr("SIMLOCK_GATEWAY_URL", "http://host.docker.internal:4700"), "/"),
		SimlockOperatorToken:    os.Getenv("SIMLOCK_OPERATOR_TOKEN"),
	}
	if raw := os.Getenv("SIGNCLOUD_STALE_BUILD_MINUTES"); raw != "" {
		minutes, err := strconv.Atoi(raw)
		if err != nil || minutes < 1 {
			return Config{}, fmt.Errorf("SIGNCLOUD_STALE_BUILD_MINUTES must be a positive integer")
		}
		cfg.StaleBuildTimeout = time.Duration(minutes) * time.Minute
	}
	if raw := os.Getenv("SIGNCLOUD_DEVICE_ENROLLMENT_ENABLED"); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("SIGNCLOUD_DEVICE_ENROLLMENT_ENABLED must be true or false")
		}
		cfg.DeviceEnrollmentEnabled = value
	}
	if raw := os.Getenv("SIGNCLOUD_MAX_DEVICES_PER_TOKEN"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 100 {
			return Config{}, fmt.Errorf("SIGNCLOUD_MAX_DEVICES_PER_TOKEN must be between 1 and 100")
		}
		cfg.MaxDevicesPerToken = value
	}
	if raw := os.Getenv("SIGNCLOUD_MAX_UPLOAD_MB"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 1 {
			return Config{}, fmt.Errorf("SIGNCLOUD_MAX_UPLOAD_MB must be a positive integer")
		}
		cfg.MaxUploadMB = value
	}
	if cfg.DatabaseURL == "" || len(cfg.APITokens) == 0 || cfg.AgentToken == "" {
		return Config{}, fmt.Errorf("DATABASE_URL, SIGNCLOUD_API_TOKEN or SIGNCLOUD_API_TOKENS, and SIGNCLOUD_AGENT_TOKEN are required")
	}
	abs, err := filepath.Abs(cfg.StorageDir)
	if err != nil {
		return Config{}, err
	}
	cfg.StorageDir = abs
	return cfg, nil
}

func tokenList(primary, additional string) []string {
	seen := make(map[string]struct{})
	var tokens []string
	for _, raw := range append([]string{primary}, strings.Split(additional, ",")...) {
		token := strings.TrimSpace(raw)
		if token == "" {
			continue
		}
		if _, exists := seen[token]; exists {
			continue
		}
		seen[token] = struct{}{}
		tokens = append(tokens, token)
	}
	return tokens
}

func (cfg Config) apiTokens() []string {
	if len(cfg.APITokens) > 0 {
		return cfg.APITokens
	}
	return tokenList(cfg.APIToken, "")
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
