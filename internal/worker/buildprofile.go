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
}

func (d declaredSettings) has(key string) bool { return d.keys[key] }

func (d declaredSettings) excludes(arch string) bool { return d.excluded[arch] }

// settingAssignment matches `KEY = value;` in project.pbxproj and
// `KEY = value` or `KEY[sdk=...] = value` in xcconfig files.
var settingAssignment = regexp.MustCompile(`(?m)^\s*"?([A-Z][A-Z0-9_]*)(\[[^\]]*\])?"?\s*=\s*(.*)$`)

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
		for _, match := range settingAssignment.FindAllStringSubmatch(string(data), -1) {
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
