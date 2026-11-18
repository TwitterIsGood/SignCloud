package worker

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func readArtifactMetadata(artifactPath, workDir string) (ArtifactMetadata, error) {
	reader, err := zip.OpenReader(artifactPath)
	if err != nil {
		return ArtifactMetadata{}, err
	}
	defer reader.Close()
	var info *zip.File
	for _, file := range reader.File {
		name := strings.TrimPrefix(file.Name, "/")
		isIPAInfo := strings.HasPrefix(name, "Payload/") && strings.Count(name, "/") == 2 && strings.HasSuffix(name, ".app/Info.plist")
		isSimulatorInfo := !strings.HasPrefix(name, "Payload/") && strings.Count(name, "/") == 1 && strings.HasSuffix(name, ".app/Info.plist")
		if isIPAInfo || isSimulatorInfo {
			info = file
			break
		}
	}
	if info == nil {
		return ArtifactMetadata{}, fmt.Errorf("application Info.plist not found in artifact")
	}
	rc, err := info.Open()
	if err != nil {
		return ArtifactMetadata{}, err
	}
	tmp := filepath.Join(workDir, "artifact-Info.plist")
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		rc.Close()
		return ArtifactMetadata{}, err
	}
	_, copyErr := io.Copy(out, rc)
	rc.Close()
	out.Close()
	if copyErr != nil {
		return ArtifactMetadata{}, copyErr
	}
	cmd := exec.CommandContext(context.Background(), "plutil", "-convert", "json", "-o", "-", tmp)
	data, err := cmd.Output()
	if err != nil {
		return ArtifactMetadata{}, err
	}
	return parseMetadata(data)
}

func readAppBundleMetadata(appPath string) (ArtifactMetadata, error) {
	infoPath := filepath.Join(appPath, "Info.plist")
	if _, err := os.Stat(infoPath); err != nil {
		return ArtifactMetadata{}, fmt.Errorf("application Info.plist not found in %s: %w", appPath, err)
	}
	data, err := exec.CommandContext(context.Background(), "plutil", "-convert", "json", "-o", "-", infoPath).Output()
	if err != nil {
		return ArtifactMetadata{}, err
	}
	return parseMetadata(data)
}

func parseMetadata(data []byte) (ArtifactMetadata, error) {
	var plist map[string]any
	if err := json.Unmarshal(data, &plist); err != nil {
		return ArtifactMetadata{}, err
	}
	return ArtifactMetadata{
		BundleID:  stringValue(plist["CFBundleIdentifier"]),
		Version:   firstNonEmpty(stringValue(plist["CFBundleShortVersionString"]), stringValue(plist["CFBundleVersion"])),
		MinimumOS: stringValue(plist["MinimumOSVersion"]),
	}, nil
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
