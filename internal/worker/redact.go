package worker

import (
	"regexp"
	"strings"
)

var sensitiveAssignment = regexp.MustCompile(`(?i)(token|password|secret|private[_ -]?key)\s*[:=]\s*[^\s,;]+`)

func (a *Agent) redact(value string) string {
	for _, secret := range []string{a.cfg.AgentToken, a.cfg.AgentDeviceToken} {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "<redacted>")
		}
	}
	for _, path := range []string{
		a.cfg.APIKeyPath, a.cfg.KeychainPath, a.cfg.KeychainPasswordFile,
		a.cfg.SigningP12Path, a.cfg.SigningP12PasswordFile,
		a.cfg.MacOSKeychainPath, a.cfg.MacOSKeychainPasswordFile,
	} {
		if path != "" {
			value = strings.ReplaceAll(value, path, "<worker-credential-path>")
		}
	}
	return sensitiveAssignment.ReplaceAllString(value, "$1=<redacted>")
}
