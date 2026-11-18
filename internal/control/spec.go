package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
)

var xcodeVersionPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+){0,2}$`)

// validateXcodeVersion accepts "26", "26.6" or "27.0.1". An empty value keeps
// the worker's xcode-select default, so older clients remain valid.
func validateXcodeVersion(version string) error {
	if version == "" {
		return nil
	}
	if !xcodeVersionPattern.MatchString(version) {
		return fmt.Errorf("spec.xcode_version %q must look like \"26\", \"26.6\" or \"27.0\"", version)
	}
	return nil
}

func parseAndValidateBuildSpec(kind string, raw []byte) (BuildSpec, error) {
	var spec BuildSpec
	if kind == "macos" {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&spec); err != nil {
			return BuildSpec{}, fmt.Errorf("invalid macos spec: %w", err)
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return BuildSpec{}, errors.New("invalid macos spec: multiple JSON values")
		}
		if err := validateXcodeVersion(spec.XcodeVersion); err != nil {
			return BuildSpec{}, err
		}
		if err := validateMacOSSpec(spec); err != nil {
			return BuildSpec{}, err
		}
		return spec, nil
	}
	if kind == "resolve" {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&spec); err != nil {
			return BuildSpec{}, fmt.Errorf("invalid resolve spec: %w", err)
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return BuildSpec{}, errors.New("invalid resolve spec: multiple JSON values")
		}
		if err := validateXcodeVersion(spec.XcodeVersion); err != nil {
			return BuildSpec{}, err
		}
		if len(spec.Environment) > 0 {
			return BuildSpec{}, errors.New("resolve does not accept environment variables")
		}
		if spec.Project != "" && spec.Workspace != "" {
			return BuildSpec{}, errors.New("resolve spec accepts at most one of project or workspace")
		}
		for _, container := range []string{spec.Project, spec.Workspace} {
			if container != "" && !safeRelativePath(container) {
				return BuildSpec{}, errors.New("resolve project/workspace must be a safe relative path")
			}
		}
		return spec, nil
	}
	if kind == "verify" {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&spec); err != nil {
			return BuildSpec{}, fmt.Errorf("invalid verify spec: %w", err)
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return BuildSpec{}, errors.New("invalid verify spec: multiple JSON values")
		}
		if err := validateXcodeVersion(spec.XcodeVersion); err != nil {
			return BuildSpec{}, err
		}
		if err := validateVerifySpec(spec); err != nil {
			return BuildSpec{}, err
		}
		return spec, nil
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return BuildSpec{}, errors.New("spec must be valid JSON")
	}
	if err := validateXcodeVersion(spec.XcodeVersion); err != nil {
		return BuildSpec{}, err
	}
	return spec, nil
}

var verifyDeviceOSPattern = regexp.MustCompile(`^[0-9.<>= -]+$`)

func validateVerifySpec(spec BuildSpec) error {
	if spec.Project != "" && spec.Workspace != "" {
		return errors.New("verify spec accepts at most one of project or workspace")
	}
	for _, container := range []string{spec.Project, spec.Workspace} {
		if container != "" && !safeRelativePath(container) {
			return errors.New("verify project/workspace must be a safe relative path")
		}
	}
	if spec.Device != "" {
		if len(spec.Device) > 100 || hasControlCharacter(spec.Device) {
			return errors.New("verify spec.device must be 1 to 100 characters without control characters")
		}
	}
	if spec.DeviceOS != "" {
		if len(spec.DeviceOS) > 40 || !verifyDeviceOSPattern.MatchString(spec.DeviceOS) || !strings.ContainsAny(spec.DeviceOS, "0123456789") {
			return errors.New("verify spec.device_os must be an OS version or range such as \"26.5\", \">=26\" or \"18 - 26\"")
		}
	}
	if spec.DeviceMode != "" && spec.DeviceMode != "slim" && spec.DeviceMode != "full" {
		return errors.New("verify spec.device_mode must be slim or full")
	}
	if spec.VerifyScript != "" {
		if !safeRelativePath(spec.VerifyScript) {
			return errors.New("verify spec.verify_script must be a safe relative path")
		}
		switch strings.ToLower(filepath.Ext(spec.VerifyScript)) {
		case ".ad", ".yaml", ".yml":
		default:
			return errors.New("verify spec.verify_script must be an .ad, .yaml or .yml file")
		}
	}
	if spec.VerifyTimeoutSecs != 0 && (spec.VerifyTimeoutSecs < 60 || spec.VerifyTimeoutSecs > 7200) {
		return errors.New("verify spec.verify_timeout_seconds must be between 60 and 7200")
	}
	if spec.Notarize || spec.Signing == "developer-id" {
		return errors.New("verify does not sign artifacts; remove notarize/developer-id signing")
	}
	return nil
}

func hasControlCharacter(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func validateMacOSSpec(spec BuildSpec) error {
	if strings.TrimSpace(spec.Scheme) == "" {
		return errors.New("macos spec.scheme is required and must name a shared Scheme")
	}
	if (spec.Project == "") == (spec.Workspace == "") {
		return errors.New("macos spec requires exactly one of project or workspace")
	}
	container := spec.Project
	if container == "" {
		container = spec.Workspace
	}
	if !safeRelativePath(container) {
		return errors.New("macos project/workspace must be a safe relative path")
	}
	if spec.Environment != nil && len(spec.Environment) > 0 {
		return errors.New("macos builds do not accept client-controlled environment variables or commands")
	}
	seen := map[string]bool{}
	architectures := spec.Architectures
	if len(architectures) == 0 {
		architectures = []string{"arm64", "x86_64"}
	}
	for _, architecture := range architectures {
		if architecture != "arm64" && architecture != "x86_64" {
			return fmt.Errorf("unsupported macos architecture %q", architecture)
		}
		if seen[architecture] {
			return fmt.Errorf("duplicate macos architecture %q", architecture)
		}
		seen[architecture] = true
	}
	signing := spec.Signing
	if signing == "" {
		signing = "adhoc"
	}
	if signing != "adhoc" && signing != "developer-id" {
		return errors.New("macos signing must be adhoc or developer-id")
	}
	if spec.Notarize && signing != "developer-id" {
		return errors.New("macos notarize=true requires signing=developer-id")
	}
	if spec.Notarize && spec.HardenedRuntime != nil && !*spec.HardenedRuntime {
		return errors.New("macos notarization requires hardened_runtime=true")
	}
	format := spec.ArtifactFormat
	if format == "" {
		format = "dmg"
	}
	if format != "dmg" && format != "app-zip" {
		return errors.New("macos artifact_format must be dmg or app-zip")
	}
	if spec.Notarize && format != "dmg" {
		return errors.New("macos notarization currently requires artifact_format=dmg")
	}
	return nil
}
