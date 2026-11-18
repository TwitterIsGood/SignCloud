package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var identityHashPattern = regexp.MustCompile(`(?i)\b[0-9a-f]{40}\b`)

func (r *jobRunner) buildMacOS(ctx context.Context, source string, spec BuildSpec) (string, ArtifactMetadata, error) {
	root := filepath.Join(r.jobDir, "source")
	if err := unzip(source, root); err != nil {
		return "", ArtifactMetadata{}, fmt.Errorf("preparing: extract source: %w", err)
	}
	return r.buildMacOSAt(ctx, collapseSingleDirectory(root), spec, filepath.Join(r.jobDir, "DerivedData"))
}

func (r *jobRunner) buildMacOSAt(ctx context.Context, root string, spec BuildSpec, derivedData string) (string, ArtifactMetadata, error) {
	r.strategy = "macos"
	r.agent.stage(ctx, r.buildID, "preparing")
	if err := validateMacOSWorkerSpec(spec); err != nil {
		return "", ArtifactMetadata{}, fmt.Errorf("preparing: %w", err)
	}
	if err := r.generateMacOSProjectIfNeeded(ctx, root, spec); err != nil {
		return "", ArtifactMetadata{}, err
	}
	r.agent.stage(ctx, r.buildID, "preparing")
	containerArgs, err := macOSContainerArgs(root, spec)
	if err != nil {
		return "", ArtifactMetadata{}, fmt.Errorf("preparing: %w", err)
	}
	if err := r.validateSharedMacOSScheme(ctx, root, containerArgs, spec.Scheme); err != nil {
		return "", ArtifactMetadata{}, fmt.Errorf("preparing: %w", err)
	}

	architectures := normalizedMacOSArchitectures(spec.Architectures)
	configuration := spec.Configuration
	if configuration == "" {
		configuration = "Release"
	}
	signing := spec.Signing
	if signing == "" {
		signing = "adhoc"
	}
	hardened := spec.HardenedRuntime == nil || *spec.HardenedRuntime
	identity := "-"
	if signing == "developer-id" {
		if err := r.unlockMacOSKeychain(); err != nil {
			return "", ArtifactMetadata{}, fmt.Errorf("preparing: %w", err)
		}
		teamID := spec.TeamID
		if teamID == "" {
			teamID = r.agent.cfg.TeamID
		}
		identity, err = r.agent.macOSDeveloperIdentity(teamID)
		if err != nil {
			return "", ArtifactMetadata{}, fmt.Errorf("preparing: %w", err)
		}
	}

	r.agent.stage(ctx, r.buildID, "building")
	xcodeEnv, err := r.agent.xcodeEnvironment()
	if err != nil {
		return "", ArtifactMetadata{}, fmt.Errorf("building: %w", err)
	}
	args := append([]string{}, containerArgs...)
	args = append(args, "-scheme", spec.Scheme, "-configuration", configuration)
	args = append(args, r.packageResolutionArgs(root)...)
	args = append(args, xcodeTrustArgs()...)
	args = append(args, "-parallelizeTargets", "-destination", "generic/platform=macOS",
		"-derivedDataPath", derivedData,
		"ARCHS="+strings.Join(architectures, " "), "ONLY_ACTIVE_ARCH=NO",
		"CODE_SIGN_STYLE=Manual", "CODE_SIGN_IDENTITY=-", "CODE_SIGNING_REQUIRED=YES", "build")
	started := time.Now()
	if err := r.command(ctx, root, r.withXcodeVersion(xcodeEnv), "xcodebuild", args...); err != nil {
		return "", ArtifactMetadata{}, fmt.Errorf("building: macOS xcodebuild failed; verify the shared Scheme, configuration, deployment target, and Universal-compatible dependencies: %w", err)
	}
	r.xcodeMS += time.Since(started).Milliseconds()
	app, err := findMacOSApp(filepath.Join(derivedData, "Build", "Products"), configuration)
	if err != nil {
		return "", ArtifactMetadata{}, fmt.Errorf("building: %w", err)
	}

	metadata, executable, err := readMacOSAppMetadata(app)
	if err != nil {
		return "", ArtifactMetadata{}, fmt.Errorf("building: %w", err)
	}
	if spec.BundleID != "" && metadata.BundleID != spec.BundleID {
		return "", ArtifactMetadata{}, fmt.Errorf("building: bundle ID mismatch: built %q, requested %q", metadata.BundleID, spec.BundleID)
	}
	if err := verifyMacOSBundledResources(root, app); err != nil {
		return "", ArtifactMetadata{}, fmt.Errorf("building: %w", err)
	}
	actualArchitectures, err := macOSArchitectures(filepath.Join(app, "Contents", "MacOS", executable))
	if err != nil {
		return "", ArtifactMetadata{}, fmt.Errorf("building: inspect application architectures: %w", err)
	}
	if missing := missingArchitectures(architectures, actualArchitectures); len(missing) > 0 {
		return "", ArtifactMetadata{}, fmt.Errorf("building: Universal architecture missing: %s; ensure all targets and dependencies support ARCHS=%q", strings.Join(missing, ","), strings.Join(architectures, " "))
	}
	metadata.Architectures = actualArchitectures

	r.agent.stage(ctx, r.buildID, "signing")
	if signing == "developer-id" {
		if err := r.unlockMacOSKeychain(); err != nil {
			return "", ArtifactMetadata{}, fmt.Errorf("signing: %w", err)
		}
	}
	if err := r.signMacOSApp(ctx, app, identity, signing, hardened); err != nil {
		return "", ArtifactMetadata{}, fmt.Errorf("signing: %w", err)
	}
	if err := r.verifyMacOSSignature(ctx, app, signing, hardened); err != nil {
		return "", ArtifactMetadata{}, fmt.Errorf("signing: %w", err)
	}
	metadata.Signing = signing
	metadata.Signed = true

	r.agent.stage(ctx, r.buildID, "packaging")
	packageStarted := time.Now()
	displayName := safeMacOSDisplayName(r.buildName)
	if displayName == "" {
		displayName = strings.TrimSuffix(filepath.Base(app), ".app")
	}
	appZip := ""
	if spec.IncludeAppZip || spec.ArtifactFormat == "app-zip" {
		appZip = filepath.Join(r.jobDir, displayName+".app.zip")
		if err := packageMacOSAppZip(ctx, r, app, displayName, appZip); err != nil {
			return "", ArtifactMetadata{}, fmt.Errorf("packaging: create app.zip: %w", err)
		}
		metadata.AppZip = appZip
	}
	artifact := appZip
	if spec.ArtifactFormat == "" || spec.ArtifactFormat == "dmg" {
		artifact = filepath.Join(r.jobDir, displayName+".dmg")
		if err := packageMacOSDMG(ctx, r, app, displayName, artifact); err != nil {
			return "", ArtifactMetadata{}, fmt.Errorf("packaging: create DMG: %w", err)
		}
	}
	r.packagingMS += time.Since(packageStarted).Milliseconds()

	if spec.Notarize {
		r.agent.stage(ctx, r.buildID, "notarizing")
		if err := r.notarizeMacOSArtifact(ctx, artifact); err != nil {
			return "", ArtifactMetadata{}, fmt.Errorf("notarizing: %w", err)
		}
		metadata.Notarized = true
		r.agent.stage(ctx, r.buildID, "stapling")
		if err := r.command(ctx, r.jobDir, r.withXcodeVersion(nil), "xcrun", "stapler", "staple", artifact); err != nil {
			return "", ArtifactMetadata{}, fmt.Errorf("stapling: Apple accepted the submission, but staple failed; retry after checking Apple ticket availability: %w", err)
		}
		metadata.Stapled = true
	}

	r.agent.stage(ctx, r.buildID, "validating")
	if err := r.finalValidateMacOS(ctx, artifact, displayName, signing, spec.Notarize); err != nil {
		return "", ArtifactMetadata{}, fmt.Errorf("validating: %w", err)
	}
	digest, err := sha256File(artifact)
	if err != nil {
		return "", ArtifactMetadata{}, fmt.Errorf("validating: calculate SHA-256: %w", err)
	}
	metadata.SHA256 = digest
	return artifact, metadata, nil
}

func validateMacOSWorkerSpec(spec BuildSpec) error {
	if strings.TrimSpace(spec.Scheme) == "" {
		return errors.New("shared Scheme is required")
	}
	if (spec.Project == "") == (spec.Workspace == "") {
		return errors.New("exactly one project or workspace is required")
	}
	container := spec.Project
	if container == "" {
		container = spec.Workspace
	}
	if !safeManifestPath(container) {
		return errors.New("project/workspace must be a safe relative path")
	}
	if len(spec.Environment) > 0 {
		return errors.New("client-controlled environment variables and commands are not allowed")
	}
	if spec.Notarize && spec.Signing != "developer-id" {
		return errors.New("notarize=true requires developer-id signing")
	}
	return nil
}

func normalizedMacOSArchitectures(values []string) []string {
	if len(values) == 0 {
		return []string{"arm64", "x86_64"}
	}
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}

func macOSContainerArgs(root string, spec BuildSpec) ([]string, error) {
	if spec.Workspace != "" {
		path := filepath.Join(root, filepath.FromSlash(spec.Workspace))
		if info, err := os.Stat(path); err != nil || !info.IsDir() || !strings.HasSuffix(path, ".xcworkspace") {
			return nil, fmt.Errorf("workspace %q was not found", spec.Workspace)
		}
		return []string{"-workspace", path}, nil
	}
	path := filepath.Join(root, filepath.FromSlash(spec.Project))
	if info, err := os.Stat(path); err != nil || !info.IsDir() || !strings.HasSuffix(path, ".xcodeproj") {
		return nil, fmt.Errorf("project %q was not found", spec.Project)
	}
	return []string{"-project", path}, nil
}

func (r *jobRunner) generateMacOSProjectIfNeeded(ctx context.Context, root string, spec BuildSpec) error {
	container := spec.Project
	if container == "" {
		container = spec.Workspace
	}
	if info, err := os.Stat(filepath.Join(root, filepath.FromSlash(container))); err == nil && info.IsDir() {
		return nil
	}
	if !fileExists(filepath.Join(root, "project.yml")) {
		return fmt.Errorf("preparing: %q was not found and project.yml is unavailable", container)
	}
	if !r.agent.cfg.MacOSXcodeGenEnabled {
		return errors.New("preparing: source contains only project.yml; worker allow-listed XcodeGen generation is disabled")
	}
	if _, err := exec.LookPath("xcodegen"); err != nil {
		return errors.New("preparing: source contains only project.yml, but XcodeGen is not installed on the worker")
	}
	r.agent.stage(ctx, r.buildID, "generating_project")
	if err := r.command(ctx, root, nil, "xcodegen", "generate"); err != nil {
		return fmt.Errorf("generating_project: xcodegen generate failed: %w", err)
	}
	return nil
}

func (r *jobRunner) validateSharedMacOSScheme(_ context.Context, root string, _ []string, scheme string) error {
	shared := false
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || !strings.HasSuffix(path, ".xcscheme") {
			return nil
		}
		if entry.IsDir() {
			return filepath.SkipDir
		}
		if strings.TrimSuffix(filepath.Base(path), ".xcscheme") == scheme && filepath.Base(filepath.Dir(path)) == "xcschemes" && filepath.Base(filepath.Dir(filepath.Dir(path))) == "xcshareddata" {
			shared = true
			return fs.SkipAll
		}
		return nil
	})
	if !shared {
		return fmt.Errorf("shared Scheme %q was not found; mark it Shared in Xcode and include xcshareddata/xcschemes", scheme)
	}
	return nil
}

func (a *Agent) macOSDeveloperIdentity(teamID string) (string, error) {
	selector := strings.TrimSpace(a.cfg.MacOSDeveloperIDIdentity)
	if selector == "" {
		return "", errors.New("Developer ID identity is not configured on this worker; configure a Developer ID Application certificate and retry")
	}
	args := []string{"find-identity", "-v", "-p", "codesigning"}
	if a.cfg.MacOSKeychainPath != "" {
		args = append(args, a.cfg.MacOSKeychainPath)
	}
	output, err := securityOutput(args...)
	if err != nil {
		return "", errors.New("Developer ID identity lookup failed; verify the dedicated build keychain")
	}
	for _, line := range strings.Split(string(output), "\n") {
		if !strings.Contains(line, "Developer ID Application:") {
			continue
		}
		hash := identityHashPattern.FindString(line)
		if hash == "" || (!strings.EqualFold(selector, hash) && !strings.Contains(line, selector)) {
			continue
		}
		if teamID != "" && !strings.Contains(line, "("+teamID+")") {
			return "", errors.New("Developer ID Team ID does not match the requested team_id; use the certificate's team or configure the correct identity")
		}
		return hash, nil
	}
	return "", errors.New("configured Developer ID Application identity was not found or is not valid")
}

func (r *jobRunner) unlockMacOSKeychain() error {
	if r.agent.cfg.MacOSKeychainPath == "" {
		return nil
	}
	if r.agent.cfg.MacOSKeychainPasswordFile == "" {
		return errors.New("Developer ID keychain password file is not configured")
	}
	password, err := readSecretFile(r.agent.cfg.MacOSKeychainPasswordFile)
	if err != nil {
		return errors.New("could not read the Developer ID keychain password")
	}
	if _, err := securityOutput("unlock-keychain", "-p", password, r.agent.cfg.MacOSKeychainPath); err != nil {
		return errors.New("could not unlock the Developer ID build keychain")
	}
	searchOutput, err := securityOutput("list-keychains", "-d", "user")
	if err != nil {
		return errors.New("could not read the build Keychain search list")
	}
	search := []string{r.agent.cfg.MacOSKeychainPath}
	for _, path := range parseKeychainList(string(searchOutput)) {
		if path != "" && path != r.agent.cfg.MacOSKeychainPath {
			search = append(search, path)
		}
	}
	args := append([]string{"list-keychains", "-d", "user", "-s"}, search...)
	if _, err := securityOutput(args...); err != nil {
		return errors.New("could not add the Developer ID Keychain to the search list")
	}
	return nil
}

func (r *jobRunner) signMacOSApp(ctx context.Context, app, identity, signing string, hardened bool) error {
	objects, err := nestedCodeObjects(app)
	if err != nil {
		return err
	}
	objects = append(objects, app)
	for _, object := range objects {
		args := []string{"--force", "--sign", identity}
		if hardened {
			args = append(args, "--options", "runtime")
		}
		if signing == "developer-id" {
			args = append(args, "--timestamp")
			if r.agent.cfg.MacOSKeychainPath != "" {
				args = append(args, "--keychain", r.agent.cfg.MacOSKeychainPath)
			}
			entitlements, entitlementsErr := sanitizedDeveloperIDEntitlements(object, r.jobDir)
			if entitlementsErr != nil {
				return fmt.Errorf("sanitize entitlements for %s: %w", filepath.Base(object), entitlementsErr)
			}
			args = append(args, "--preserve-metadata=identifier,requirements")
			if entitlements != "" {
				args = append(args, "--entitlements", entitlements)
			}
		} else {
			args = append(args, "--timestamp=none")
			args = append(args, "--preserve-metadata=identifier,entitlements,requirements")
		}
		args = append(args, object)
		if err := r.command(ctx, r.jobDir, nil, "/usr/bin/codesign", args...); err != nil {
			return fmt.Errorf("sign %s failed: %w", filepath.Base(object), err)
		}
	}
	return nil
}

func sanitizedDeveloperIDEntitlements(object, jobDir string) (string, error) {
	command := exec.Command("/usr/bin/codesign", "--display", "--entitlements", ":-", object)
	output, err := command.Output()
	if err != nil && len(output) == 0 {
		return "", nil
	}
	if !strings.Contains(string(output), "<plist") {
		return "", nil
	}
	file, err := os.CreateTemp(jobDir, "developer-id-entitlements-*.plist")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if _, err := file.Write(output); err != nil {
		file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	_ = exec.Command("/usr/libexec/PlistBuddy", "-c", "Delete :com.apple.security.get-task-allow", path).Run()
	if err := exec.Command("/usr/bin/plutil", "-lint", path).Run(); err != nil {
		return "", errors.New("sanitized entitlements are not a valid property list")
	}
	return path, nil
}

func (r *jobRunner) verifyMacOSSignature(ctx context.Context, app, signing string, hardened bool) error {
	if err := r.command(ctx, r.jobDir, nil, "/usr/bin/codesign", "--verify", "--deep", "--strict", "--verbose=2", app); err != nil {
		return fmt.Errorf("codesign deep strict verification failed: %w", err)
	}
	output, err := exec.Command("/usr/bin/codesign", "--display", "--verbose=4", app).CombinedOutput()
	if err != nil {
		return errors.New("could not inspect the final application signature")
	}
	if hardened && !strings.Contains(string(output), "runtime") {
		return errors.New("Hardened Runtime verification failed: runtime flag is absent")
	}
	entitlements, _ := exec.Command("/usr/bin/codesign", "--display", "--entitlements", ":-", app).CombinedOutput()
	if signing == "developer-id" && strings.Contains(string(entitlements), "com.apple.security.get-task-allow") {
		return errors.New("Developer ID validation failed: com.apple.security.get-task-allow is present")
	}
	sandbox := strings.Contains(string(entitlements), "com.apple.security.app-sandbox") && strings.Contains(string(entitlements), "<true/>")
	r.agent.log(ctx, r.buildID, fmt.Sprintf("signature verification passed; hardened_runtime=%t app_sandbox=%t\n", hardened, sandbox))
	return nil
}

func findMacOSApp(root, configuration string) (string, error) {
	var apps []string
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.IsDir() {
			return nil
		}
		if strings.HasSuffix(entry.Name(), ".app") && !strings.Contains(strings.ToLower(path), "test") {
			apps = append(apps, path)
			return filepath.SkipDir
		}
		return nil
	})
	sort.Strings(apps)
	for _, app := range apps {
		if strings.Contains(app, configuration) {
			return app, nil
		}
	}
	if len(apps) > 0 {
		return apps[0], nil
	}
	return "", errors.New("no macOS .app product was found")
}

func readMacOSAppMetadata(app string) (ArtifactMetadata, string, error) {
	info := filepath.Join(app, "Contents", "Info.plist")
	output, err := exec.Command("plutil", "-convert", "json", "-o", "-", info).Output()
	if err != nil {
		return ArtifactMetadata{}, "", errors.New("application Info.plist could not be read")
	}
	var values map[string]any
	if err := json.Unmarshal(output, &values); err != nil {
		return ArtifactMetadata{}, "", err
	}
	executable := stringValue(values["CFBundleExecutable"])
	if executable == "" {
		return ArtifactMetadata{}, "", errors.New("CFBundleExecutable is missing")
	}
	return ArtifactMetadata{
		BundleID:    stringValue(values["CFBundleIdentifier"]),
		Version:     firstNonEmpty(stringValue(values["CFBundleShortVersionString"]), stringValue(values["CFBundleVersion"])),
		BuildNumber: stringValue(values["CFBundleVersion"]),
		MinimumOS:   firstNonEmpty(stringValue(values["LSMinimumSystemVersion"]), stringValue(values["MinimumOSVersion"])),
	}, executable, nil
}

func macOSArchitectures(executable string) ([]string, error) {
	output, err := exec.Command("/usr/bin/lipo", "-archs", executable).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("lipo failed: %s", strings.TrimSpace(string(output)))
	}
	architectures := strings.Fields(string(output))
	sort.Strings(architectures)
	return architectures, nil
}

func missingArchitectures(expected, actual []string) []string {
	available := map[string]bool{}
	for _, value := range actual {
		available[value] = true
	}
	var missing []string
	for _, value := range expected {
		if !available[value] {
			missing = append(missing, value)
		}
	}
	return missing
}

func verifyMacOSBundledResources(root, app string) error {
	localizations := map[string]bool{}
	hasAppIcon := false
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() && path != root {
			switch entry.Name() {
			case ".git", ".build", "DerivedData", "Pods", "node_modules", "build":
				return filepath.SkipDir
			}
			lower := strings.ToLower(entry.Name())
			if strings.HasSuffix(lower, ".lproj") {
				localizations[entry.Name()] = true
				return filepath.SkipDir
			}
			if lower == "appicon.appiconset" {
				hasAppIcon = true
				return filepath.SkipDir
			}
		}
		return nil
	})
	resources := filepath.Join(app, "Contents", "Resources")
	for localization := range localizations {
		if info, err := os.Stat(filepath.Join(resources, localization)); err != nil || !info.IsDir() {
			return fmt.Errorf("localized resource %s is missing from the application bundle; add it to the target's Resources build phase", localization)
		}
	}
	if hasAppIcon && !fileExists(filepath.Join(resources, "Assets.car")) {
		return errors.New("AppIcon assets are missing from the application bundle; add the asset catalog to the target's Resources build phase")
	}
	return nil
}

func safeMacOSDisplayName(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Map(func(r rune) rune {
		if r == '/' || r == ':' || r == 0 || r == '\n' || r == '\r' {
			return '-'
		}
		return r
	}, value)
	value = strings.Trim(strings.TrimSuffix(value, ".app"), ". ")
	runes := []rune(value)
	if len(runes) > 80 {
		value = string(runes[:80])
	}
	return value
}

func packageMacOSAppZip(ctx context.Context, r *jobRunner, app, displayName, output string) error {
	root := filepath.Join(r.jobDir, "app-zip-root")
	if err := os.RemoveAll(root); err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return err
	}
	if err := r.command(ctx, root, nil, "/usr/bin/ditto", app, filepath.Join(root, displayName+".app")); err != nil {
		return err
	}
	return r.command(ctx, root, nil, "/usr/bin/ditto", "-c", "-k", "--sequesterRsrc", "--keepParent", displayName+".app", output)
}

func packageMacOSDMG(ctx context.Context, r *jobRunner, app, displayName, output string) error {
	root := filepath.Join(r.jobDir, "dmg-root")
	if err := os.RemoveAll(root); err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return err
	}
	if err := r.command(ctx, root, nil, "/usr/bin/ditto", app, filepath.Join(root, displayName+".app")); err != nil {
		return err
	}
	if err := os.Symlink("/Applications", filepath.Join(root, "Applications")); err != nil {
		return err
	}
	return r.command(ctx, r.jobDir, nil, "/usr/bin/hdiutil", "create", "-ov", "-format", "UDZO", "-volname", displayName, "-srcfolder", root, output)
}

func (r *jobRunner) notarizeMacOSArtifact(ctx context.Context, artifact string) error {
	profile := strings.TrimSpace(r.agent.cfg.MacOSNotaryProfile)
	if profile == "" {
		return errors.New("notarization credentials are not configured on this worker; configure a worker-side notarytool keychain profile and retry")
	}
	args := []string{"notarytool", "submit", artifact, "--keychain-profile", profile, "--wait", "--output-format", "json"}
	if r.agent.cfg.MacOSKeychainPath != "" {
		args = append(args, "--keychain", r.agent.cfg.MacOSKeychainPath)
	}
	output, err := r.commandOutput(ctx, r.jobDir, r.withXcodeVersion(nil), "xcrun", args...)
	if err != nil {
		return errors.New("notarytool submission failed; verify network access and the worker-side notarization profile")
	}
	var result struct {
		ID      string `json:"id"`
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		return errors.New("could not parse Apple notarization result")
	}
	if result.Status != "Accepted" {
		summary := strings.TrimSpace(result.Message)
		if result.ID != "" {
			if issues := r.notarizationIssueSummary(ctx, result.ID); issues != "" {
				summary = issues
			}
		}
		if summary == "" {
			summary = "Apple rejected the submission"
		}
		return fmt.Errorf("Apple notarization status %s: %s; inspect the signed bundle and entitlements, then submit a new build", result.Status, summary)
	}
	r.agent.log(ctx, r.buildID, "Apple notarization status: Accepted\n")
	return nil
}

func (r *jobRunner) notarizationIssueSummary(ctx context.Context, submissionID string) string {
	args := []string{"notarytool", "log", submissionID, "--keychain-profile", r.agent.cfg.MacOSNotaryProfile, "--output-format", "json"}
	if r.agent.cfg.MacOSKeychainPath != "" {
		args = append(args, "--keychain", r.agent.cfg.MacOSKeychainPath)
	}
	command := exec.CommandContext(ctx, "xcrun", args...)
	command.Dir = r.jobDir
	command.Env = childEnvironment(r.withXcodeVersion(nil))
	output, err := command.Output()
	if err != nil {
		return ""
	}
	var logResult struct {
		Issues []struct {
			Severity string `json:"severity"`
			Message  string `json:"message"`
		} `json:"issues"`
	}
	if json.Unmarshal(output, &logResult) != nil {
		return ""
	}
	var messages []string
	seen := map[string]bool{}
	for _, issue := range logResult.Issues {
		message := strings.TrimSpace(issue.Message)
		if message == "" || seen[message] {
			continue
		}
		seen[message] = true
		messages = append(messages, issue.Severity+": "+message)
		if len(messages) == 3 {
			break
		}
	}
	return strings.Join(messages, "; ")
}

func (r *jobRunner) finalValidateMacOS(ctx context.Context, artifact, displayName, signing string, notarized bool) error {
	if strings.HasSuffix(strings.ToLower(artifact), ".zip") {
		return nil
	}
	if notarized {
		if err := r.command(ctx, r.jobDir, r.withXcodeVersion(nil), "xcrun", "stapler", "validate", artifact); err != nil {
			return fmt.Errorf("stapler validate failed: %w", err)
		}
	}
	if err := r.command(ctx, r.jobDir, nil, "/usr/bin/hdiutil", "verify", artifact); err != nil {
		return fmt.Errorf("DMG verification failed: %w", err)
	}
	mount := filepath.Join(r.jobDir, "mounted-dmg")
	if err := os.MkdirAll(mount, 0o750); err != nil {
		return err
	}
	if err := r.command(ctx, r.jobDir, nil, "/usr/bin/hdiutil", "attach", "-nobrowse", "-readonly", "-mountpoint", mount, artifact); err != nil {
		return fmt.Errorf("DMG mount failed: %w", err)
	}
	defer exec.Command("/usr/bin/hdiutil", "detach", mount).Run()
	app := filepath.Join(mount, displayName+".app")
	if info, err := os.Stat(app); err != nil || !info.IsDir() {
		return fmt.Errorf("DMG does not contain %s.app", displayName)
	}
	applications := filepath.Join(mount, "Applications")
	link, err := os.Readlink(applications)
	if err != nil || link != "/Applications" {
		return errors.New("DMG Applications shortcut is missing or invalid")
	}
	if err := r.command(ctx, r.jobDir, nil, "/usr/bin/codesign", "--verify", "--deep", "--strict", "--verbose=2", app); err != nil {
		return fmt.Errorf("mounted application codesign verification failed: %w", err)
	}
	if signing == "developer-id" && notarized {
		if err := r.command(ctx, r.jobDir, nil, "/usr/sbin/spctl", "--assess", "--type", "execute", "--verbose=4", app); err != nil {
			return fmt.Errorf("Gatekeeper spctl assessment failed: %w", err)
		}
	}
	return nil
}

func sha256File(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := file.WriteTo(hasher); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}
