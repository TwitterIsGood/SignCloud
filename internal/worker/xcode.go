package worker

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// XcodeInstall is a locally installed Xcode.app with its short version string.
type XcodeInstall struct {
	Version string
	Path    string
}

func (i XcodeInstall) developerDir() string {
	return filepath.Join(i.Path, "Contents", "Developer")
}

// availableXcodes discovers installed Xcodes from SIGNCLOUD_XCODE_PATHS (glob
// patterns allowed) or, by default, /Applications plus the active xcode-select
// installation. The result is cached, but a miss triggers a refresh so an Xcode
// installed after the agent started is picked up without a restart.
func (a *Agent) availableXcodes() ([]XcodeInstall, error) {
	a.xcodesMu.Lock()
	defer a.xcodesMu.Unlock()
	if !a.xcodesLoaded {
		a.xcodes, a.xcodesErr = discoverXcodes(a.cfg.XcodePaths)
		a.xcodesLoaded = a.xcodesErr == nil
	}
	return a.xcodes, a.xcodesErr
}

func (a *Agent) refreshXcodes() ([]XcodeInstall, error) {
	installs, err := discoverXcodes(a.cfg.XcodePaths)
	a.xcodesMu.Lock()
	a.xcodes, a.xcodesErr = installs, err
	a.xcodesLoaded = err == nil
	a.xcodesMu.Unlock()
	return installs, err
}

func discoverXcodes(patterns []string) ([]XcodeInstall, error) {
	if len(patterns) == 0 {
		patterns = []string{"/Applications/Xcode*.app"}
		if output, err := exec.Command("xcode-select", "-p").Output(); err == nil {
			developerDir := strings.TrimSpace(string(output))
			if app := strings.TrimSuffix(developerDir, "/Contents/Developer"); app != developerDir {
				patterns = append(patterns, app)
			}
		}
	}
	seen := map[string]bool{}
	var installs []XcodeInstall
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid SIGNCLOUD_XCODE_PATHS pattern %q: %w", pattern, err)
		}
		for _, path := range matches {
			absolute, err := filepath.Abs(path)
			if err != nil || seen[absolute] {
				continue
			}
			version, err := xcodeShortVersion(absolute)
			if err != nil {
				continue
			}
			seen[absolute] = true
			installs = append(installs, XcodeInstall{Version: version, Path: absolute})
		}
	}
	sort.Slice(installs, func(i, j int) bool {
		if cmp := compareXcodeVersions(installs[i].Version, installs[j].Version); cmp != 0 {
			return cmp > 0
		}
		return installs[i].Path < installs[j].Path
	})
	return installs, nil
}

func xcodeShortVersion(path string) (string, error) {
	versionFile := filepath.Join(path, "Contents", "version.plist")
	output, err := exec.Command("/usr/libexec/PlistBuddy", "-c", "Print :CFBundleShortVersionString", versionFile).Output()
	if err != nil {
		return "", err
	}
	version := strings.TrimSpace(string(output))
	if version == "" {
		return "", fmt.Errorf("empty CFBundleShortVersionString in %s", versionFile)
	}
	return version, nil
}

// xcodeDeveloperDir maps a requested version (e.g. "27", "26.6") to the
// matching installed Xcode. An empty request keeps the system default so
// existing builds continue to use whatever xcode-select points at.
func (a *Agent) xcodeDeveloperDir(requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", nil
	}
	installs, err := a.availableXcodes()
	if err != nil {
		return "", err
	}
	matches := matchXcodeInstalls(installs, requested)
	if len(matches) == 0 {
		// The agent may have been running before Xcode was installed.
		installs, err = a.refreshXcodes()
		if err != nil {
			return "", err
		}
		matches = matchXcodeInstalls(installs, requested)
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("xcode_version %q is not installed on this worker; available: %s",
			requested, formatXcodeInstalls(installs))
	}
	// matches is sorted newest first.
	return matches[0].developerDir(), nil
}

func matchXcodeInstalls(installs []XcodeInstall, requested string) []XcodeInstall {
	var matches []XcodeInstall
	for _, install := range installs {
		if install.Version == requested || strings.HasPrefix(install.Version, requested+".") {
			matches = append(matches, install)
		}
	}
	return matches
}

// xcodeVersionForDeveloperDir returns the short version for a DEVELOPER_DIR so
// build logs and caches can name the toolchain that was used.
func (a *Agent) xcodeVersionForDeveloperDir(developerDir string) string {
	if developerDir == "" {
		return ""
	}
	installs, err := a.availableXcodes()
	if err != nil {
		return ""
	}
	for _, install := range installs {
		if install.developerDir() == developerDir {
			return install.Version
		}
	}
	return ""
}

func formatXcodeInstalls(installs []XcodeInstall) string {
	if len(installs) == 0 {
		return "none"
	}
	versions := make([]string, 0, len(installs))
	for _, install := range installs {
		versions = append(versions, install.Version)
	}
	return strings.Join(versions, ", ")
}

func compareXcodeVersions(left, right string) int {
	leftParts := strings.Split(left, ".")
	rightParts := strings.Split(right, ".")
	for i := 0; i < len(leftParts) || i < len(rightParts); i++ {
		leftValue := 0
		rightValue := 0
		if i < len(leftParts) {
			leftValue, _ = strconv.Atoi(leftParts[i])
		}
		if i < len(rightParts) {
			rightValue, _ = strconv.Atoi(rightParts[i])
		}
		if leftValue != rightValue {
			if leftValue < rightValue {
				return -1
			}
			return 1
		}
	}
	return 0
}

func xcodePathsFromEnv(value string) []string {
	var paths []string
	for _, entry := range filepath.SplitList(value) {
		if entry = strings.TrimSpace(entry); entry != "" {
			paths = append(paths, entry)
		}
	}
	return paths
}
