package worker

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type ephemeralKeychainSession struct {
	path            string
	previousDefault string
	previousSearch  []string
	// xcodebuild runs with CFFIXED_USER_HOME set to the agent's build home, so
	// codesign started by build scripts (CocoaPods "Embed Pods Frameworks")
	// reads the keychain search list from there, not from the real user. The
	// session therefore installs the keychain in that preference domain too.
	buildHomeEnv             []string
	buildHomePreviousDefault string
	buildHomePreviousSearch  []string
}

func (a *Agent) beginEphemeralKeychain(jobDir string) (*ephemeralKeychainSession, error) {
	if !fileExists(a.cfg.SigningP12Path) {
		return nil, fmt.Errorf("signing P12 does not exist: %s", a.cfg.SigningP12Path)
	}
	p12Password, err := readSecretFile(a.cfg.SigningP12PasswordFile)
	if err != nil {
		return nil, fmt.Errorf("read signing P12 password: %w", err)
	}
	keychainPassword, err := randomSecret(32)
	if err != nil {
		return nil, fmt.Errorf("generate keychain password: %w", err)
	}

	session := &ephemeralKeychainSession{path: filepath.Join(jobDir, "signing.keychain-db")}
	if output, commandErr := securityOutput("default-keychain", "-d", "user"); commandErr == nil {
		session.previousDefault = parseKeychainPath(string(output))
	}
	if output, commandErr := securityOutput("list-keychains", "-d", "user"); commandErr == nil {
		session.previousSearch = parseKeychainList(string(output))
	}

	keychainSearch := []string{"list-keychains", "-d", "user", "-s", session.path}
	for _, path := range session.previousSearch {
		if path != "" && path != session.path {
			keychainSearch = append(keychainSearch, path)
		}
	}
	commands := [][]string{
		{"create-keychain", "-p", keychainPassword, session.path},
		{"set-keychain-settings", "-lut", "3600", session.path},
		{"unlock-keychain", "-p", keychainPassword, session.path},
		{"import", a.cfg.SigningP12Path, "-k", session.path, "-P", p12Password, "-T", "/usr/bin/codesign", "-T", "/usr/bin/security"},
		{"set-key-partition-list", "-S", "apple-tool:,apple:,codesign:", "-s", "-k", keychainPassword, session.path},
		keychainSearch,
		{"default-keychain", "-d", "user", "-s", session.path},
	}
	for _, args := range commands {
		if _, err := securityOutput(args...); err != nil {
			_ = session.Close()
			return nil, err
		}
	}
	if environment, envErr := a.xcodeEnvironment(); envErr == nil {
		session.buildHomeEnv = environment
		if output, commandErr := securityOutputEnv(environment, "default-keychain", "-d", "user"); commandErr == nil {
			session.buildHomePreviousDefault = parseKeychainPath(string(output))
		}
		if output, commandErr := securityOutputEnv(environment, "list-keychains", "-d", "user"); commandErr == nil {
			session.buildHomePreviousSearch = parseKeychainList(string(output))
		}
		buildHomeSearch := []string{"list-keychains", "-d", "user", "-s", session.path}
		for _, path := range session.buildHomePreviousSearch {
			if path != "" && path != session.path {
				buildHomeSearch = append(buildHomeSearch, path)
			}
		}
		for _, args := range [][]string{buildHomeSearch, {"default-keychain", "-d", "user", "-s", session.path}} {
			if _, err := securityOutputEnv(environment, args...); err != nil {
				_ = session.Close()
				return nil, fmt.Errorf("install ephemeral keychain for build home: %w", err)
			}
		}
	}
	identities, err := securityOutput("find-identity", "-v", "-p", "codesigning", session.path)
	if err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("verify signing identity: %w", err)
	}
	identityOutput := string(identities)
	if strings.Contains(identityOutput, "0 valid identities found") {
		_ = session.Close()
		return nil, fmt.Errorf("no valid signing identity is present in the ephemeral keychain")
	}
	return session, nil
}

func (s *ephemeralKeychainSession) Close() error {
	if s == nil || s.path == "" {
		return nil
	}
	var cleanupErrors []string
	if len(s.previousSearch) > 0 {
		args := append([]string{"list-keychains", "-d", "user", "-s"}, s.previousSearch...)
		if _, err := securityOutput(args...); err != nil {
			cleanupErrors = append(cleanupErrors, "restore search list: "+err.Error())
		}
	}
	if s.previousDefault != "" {
		if _, err := securityOutput("default-keychain", "-d", "user", "-s", s.previousDefault); err != nil {
			cleanupErrors = append(cleanupErrors, "restore default keychain: "+err.Error())
		}
	}
	if s.buildHomeEnv != nil {
		search := []string{"list-keychains", "-d", "user", "-s"}
		for _, path := range s.buildHomePreviousSearch {
			if path != "" && path != s.path {
				search = append(search, path)
			}
		}
		if _, err := securityOutputEnv(s.buildHomeEnv, search...); err != nil {
			cleanupErrors = append(cleanupErrors, "restore build home search list: "+err.Error())
		}
		if s.buildHomePreviousDefault != "" && s.buildHomePreviousDefault != s.path {
			if _, err := securityOutputEnv(s.buildHomeEnv, "default-keychain", "-d", "user", "-s", s.buildHomePreviousDefault); err != nil {
				cleanupErrors = append(cleanupErrors, "restore build home default keychain: "+err.Error())
			}
		}
	}
	if _, err := securityOutput("delete-keychain", s.path); err != nil {
		cleanupErrors = append(cleanupErrors, "delete keychain: "+err.Error())
		_ = os.Remove(s.path)
	}
	s.path = ""
	if len(cleanupErrors) > 0 {
		return fmt.Errorf("%s", strings.Join(cleanupErrors, "; "))
	}
	return nil
}

func securityOutput(args ...string) ([]byte, error) {
	return securityOutputEnv(nil, args...)
}

// securityOutputEnv runs security with environment overrides, such as the
// build home whose preference domain holds the keychain search list.
func securityOutputEnv(environment []string, args ...string) ([]byte, error) {
	command := exec.Command("/usr/bin/security", args...)
	if environment != nil {
		command.Env = childEnvironment(environment)
	}
	output, err := command.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("security %s failed: %w: %s", args[0], err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func readSecretFile(path string) (string, error) {
	value, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	secret := strings.TrimSpace(string(value))
	if secret == "" {
		return "", fmt.Errorf("secret file is empty")
	}
	return secret, nil
}

func randomSecret(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func parseKeychainList(output string) []string {
	lines := strings.Fields(output)
	paths := make([]string, 0, len(lines))
	for _, line := range lines {
		if path := parseKeychainPath(line); path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

func parseKeychainPath(value string) string {
	return strings.Trim(strings.TrimSpace(value), "\"")
}
