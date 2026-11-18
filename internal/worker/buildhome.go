package worker

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func (a *Agent) xcodeEnvironment() ([]string, error) {
	a.buildHomeOnce.Do(func() {
		a.buildHome = filepath.Join(a.cfg.CacheRoot, "build-home")
		for _, path := range []string{
			filepath.Join(a.buildHome, "Library", "Preferences"),
			filepath.Join(a.buildHome, "Library", "Developer", "Xcode", "UserData", "Provisioning Profiles"),
			filepath.Join(a.buildHome, "Library", "MobileDevice", "Provisioning Profiles"),
			filepath.Join(a.buildHome, "Library", "org.swift.swiftpm", "configuration"),
		} {
			if err := os.MkdirAll(path, 0o700); err != nil {
				a.buildHomeErr = err
				return
			}
		}
		realHome, err := os.UserHomeDir()
		if err != nil {
			a.buildHomeErr = err
			return
		}
		if !a.cfg.EphemeralKeychain {
			loginKeychain := filepath.Join(realHome, "Library", "Keychains", "login.keychain-db")
			keychain := a.cfg.KeychainPath
			if keychain == "" {
				keychain = loginKeychain
			}
			commands := make([][]string, 0, 4)
			if a.cfg.KeychainPasswordFile != "" {
				password, readErr := os.ReadFile(a.cfg.KeychainPasswordFile)
				if readErr != nil {
					a.buildHomeErr = fmt.Errorf("read build keychain password: %w", readErr)
					return
				}
				commands = append(commands,
					[]string{"unlock-keychain", "-p", strings.TrimSpace(string(password)), keychain},
					[]string{"set-keychain-settings", "-lut", "21600", keychain},
				)
			}
			searchList := []string{"list-keychains", "-d", "user", "-s", keychain}
			if keychain != loginKeychain {
				searchList = append(searchList, loginKeychain)
			}
			commands = append(commands,
				searchList,
				[]string{"default-keychain", "-d", "user", "-s", keychain},
			)
			for _, args := range commands {
				command := exec.Command("security", args...)
				command.Env = childEnvironment([]string{"HOME=" + a.buildHome, "CFFIXED_USER_HOME=" + a.buildHome})
				if output, err := command.CombinedOutput(); err != nil {
					a.buildHomeErr = fmt.Errorf("configure build keychain: %w: %s", err, output)
					return
				}
			}
		}
		profileLocations := [][2]string{
			{filepath.Join(realHome, "Library", "Developer", "Xcode", "UserData", "Provisioning Profiles"), filepath.Join(a.buildHome, "Library", "Developer", "Xcode", "UserData", "Provisioning Profiles")},
			{filepath.Join(realHome, "Library", "MobileDevice", "Provisioning Profiles"), filepath.Join(a.buildHome, "Library", "MobileDevice", "Provisioning Profiles")},
		}
		for _, location := range profileLocations {
			entries, _ := os.ReadDir(location[0])
			for _, entry := range entries {
				if entry.IsDir() || filepath.Ext(entry.Name()) != ".mobileprovision" {
					continue
				}
				if err := copyFile(filepath.Join(location[0], entry.Name()), filepath.Join(location[1], entry.Name())); err != nil {
					a.buildHomeErr = fmt.Errorf("copy provisioning profile: %w", err)
					return
				}
			}
		}
	})
	if a.buildHomeErr != nil {
		return nil, a.buildHomeErr
	}
	// Keep both home variables on the same local root. The graphical user's
	// SwiftPM cache may live on a removable volume that launchd cannot access,
	// and mixing home roots makes SwiftPM construct invalid duplicate paths.
	return []string{"HOME=" + a.buildHome, "CFFIXED_USER_HOME=" + a.buildHome}, nil
}

func (a *Agent) signingEnvironment() ([]string, error) {
	if _, err := a.xcodeEnvironment(); err != nil {
		return nil, err
	}
	realHome, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	// Xcode resolves installed provisioning profiles from the graphical user's
	// real home directory even when HOME is overridden for dependency caches.
	return []string{
		"HOME=" + realHome,
		"CFFIXED_USER_HOME=" + a.buildHome,
		"HTTP_PROXY=", "HTTPS_PROXY=", "http_proxy=", "https_proxy=",
	}, nil
}
