package worker

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Build profiles
//
// build_profile "fast" is a caller-declared platform profile for Debug
// Development and Simulator builds. It removes work whose output nobody uses
// on a build worker, and never changes Release/Archive builds:
//
//   - COMPILER_INDEX_STORE_ENABLE=NO: the index store only serves Xcode's
//     editor; measured at about 20% of a warm Debug compile.
//   - DEBUG_INFORMATION_FORMAT=dwarf: skips the dSYM bundle, a separate copy
//     of the debug symbols that only crash symbolication of a shipped build
//     needs; debugging still works from the symbols in the object files.
//   - ARCHS=<simulator_arch> (Simulator only, when the caller names the arch
//     its Simulator runs): a generic Simulator destination otherwise compiles
//     both arm64 and x86_64, doubling Swift compile work.
//
// A setting is skipped when the caller's project or xcconfig files set it
// explicitly, and ARCHS is skipped when any project excludes the requested
// arch, so a declared project choice always wins over the profile.

const (
	buildProfileStandard = "standard"
	buildProfileFast     = "fast"
)

var simulatorArchitectures = map[string]bool{"arm64": true, "x86_64": true}

type profileTarget int

const (
	profileSimulator profileTarget = iota
	profileDevice
)

type profileDecision struct {
	settings []string
	notes    []string
}

func (r *jobRunner) decideProfile(root string, spec BuildSpec, configuration string, target profileTarget) profileDecision {
	var decision profileDecision
	if spec.BuildProfile != buildProfileFast {
		return decision
	}
	if !strings.EqualFold(configuration, "Debug") {
		decision.notes = append(decision.notes, "fast profile skipped: configuration "+configuration+" is not Debug")
		return decision
	}
	declared := declaredBuildSettings(root)
	if declared.has("COMPILER_INDEX_STORE_ENABLE") {
		decision.notes = append(decision.notes, "COMPILER_INDEX_STORE_ENABLE kept: set by the project")
	} else {
		decision.settings = append(decision.settings, "COMPILER_INDEX_STORE_ENABLE=NO")
	}
	if declared.debugInfoOverride {
		decision.notes = append(decision.notes, "DEBUG_INFORMATION_FORMAT kept: set by the project for Debug")
	} else {
		decision.settings = append(decision.settings, "DEBUG_INFORMATION_FORMAT=dwarf")
	}
	if target == profileSimulator && spec.SimulatorArch != "" {
		arch := spec.SimulatorArch
		switch {
		case !simulatorArchitectures[arch]:
			decision.notes = append(decision.notes, "ARCHS kept: unsupported simulator_arch "+arch)
		case declared.has("ARCHS"):
			decision.notes = append(decision.notes, "ARCHS kept: set by the project")
		case declared.excludes(arch):
			decision.notes = append(decision.notes, "ARCHS kept: the project excludes "+arch)
		default:
			decision.settings = append(decision.settings, "ARCHS="+arch)
		}
	}
	return decision
}

// applyBuildProfile appends the profile settings to an xcodebuild argument
// list and records the outcome for metrics and the build log.
func (r *jobRunner) applyBuildProfile(ctx context.Context, args []string, root string, spec BuildSpec, configuration string, target profileTarget) []string {
	decision := r.decideProfile(root, spec, configuration, target)
	if spec.BuildProfile == buildProfileFast {
		r.buildProfile = buildProfileFast
		r.buildProfileApplied = decision.settings
		r.buildProfileNotes = decision.notes
		message := "Build profile fast: " + strings.Join(decision.settings, " ")
		if len(decision.notes) > 0 {
			message += " (" + strings.Join(decision.notes, "; ") + ")"
		}
		r.agent.log(ctx, r.buildID, message+"\n")
	}
	return append(args, decision.settings...)
}

type declaredSettings struct {
	keys     map[string]bool
	excluded map[string]bool
	// debugInfoOverride is set when a Debug configuration (or an xcconfig
	// that is not Release-only) asks for a debug information format other
	// than plain dwarf. Release declarations do not count: the profile only
	// runs for Debug, and CocoaPods declares dwarf-with-dsym for Release.
	debugInfoOverride bool
}

func (d declaredSettings) has(key string) bool { return d.keys[key] }

func (d declaredSettings) excludes(arch string) bool { return d.excluded[arch] }

// settingAssignment matches `KEY = value;` in project.pbxproj and
// `KEY = value` or `KEY[sdk=...] = value` in xcconfig files.
var settingAssignment = regexp.MustCompile(`(?m)^\s*"?([A-Z][A-Z0-9_]*)(\[[^\]]*\])?"?\s*=\s*(.*)$`)

// buildConfiguration matches one XCBuildConfiguration block of a pbxproj.
var buildConfiguration = regexp.MustCompile(`(?s)isa = XCBuildConfiguration;\s*buildSettings = \{(.*?)\n\s*\};\s*name = "?([^";]+)"?;`)

var debugInfoAssignment = regexp.MustCompile(`(?m)^\s*"?DEBUG_INFORMATION_FORMAT(\[[^\]]*\])?"?\s*=\s*"?([^";\n]*)"?`)

// debugInfoNonDwarf reports whether text sets DEBUG_INFORMATION_FORMAT to
// anything other than dwarf.
func debugInfoNonDwarf(text string) bool {
	for _, match := range debugInfoAssignment.FindAllStringSubmatch(text, -1) {
		if strings.TrimSpace(match[2]) != "dwarf" {
			return true
		}
	}
	return false
}

var profileKeys = map[string]bool{
	"ARCHS":                       true,
	"EXCLUDED_ARCHS":              true,
	"COMPILER_INDEX_STORE_ENABLE": true,
}

// declaredBuildSettings scans project.pbxproj and .xcconfig files under root
// (including Pods) for the settings the fast profile could touch.
func declaredBuildSettings(root string) declaredSettings {
	declared := declaredSettings{keys: map[string]bool{}, excluded: map[string]bool{}}
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "DerivedData", "build":
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() != "project.pbxproj" && filepath.Ext(entry.Name()) != ".xcconfig" {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		text := string(data)
		if entry.Name() == "project.pbxproj" {
			for _, block := range buildConfiguration.FindAllStringSubmatch(text, -1) {
				if strings.EqualFold(block[2], "Debug") && debugInfoNonDwarf(block[1]) {
					declared.debugInfoOverride = true
				}
			}
		} else if !strings.Contains(strings.ToLower(entry.Name()), "release") && debugInfoNonDwarf(text) {
			declared.debugInfoOverride = true
		}
		for _, match := range settingAssignment.FindAllStringSubmatch(text, -1) {
			key := match[1]
			if !profileKeys[key] {
				continue
			}
			declared.keys[key] = true
			if key == "EXCLUDED_ARCHS" {
				value := strings.Trim(strings.TrimSpace(match[3]), `;"()`)
				for _, arch := range strings.FieldsFunc(value, func(r rune) bool {
					return r == ' ' || r == ',' || r == '"' || r == '\t' || r == '\n'
				}) {
					declared.excluded[arch] = true
				}
			}
		}
		return nil
	})
	return declared
}
