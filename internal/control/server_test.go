package control

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidateManifestAcceptsSymlinkEntries(t *testing.T) {
	files := []ManifestFile{
		{Path: "Frameworks/Lib.xcframework/ios-arm64/Lib.framework/Lib", Link: "Versions/Current/Lib"},
		{Path: "App/app.txt", Hash: strings.Repeat("a", 64), Size: 4, Mode: 0o644},
	}
	if err := validateManifest("key", files); err != nil {
		t.Fatalf("symlink manifest rejected: %v", err)
	}
}

func TestValidateManifestRejectsEscapingSymlink(t *testing.T) {
	err := validateManifest("key", []ManifestFile{{Path: "Lib", Link: "../../etc/passwd"}})
	if err == nil {
		t.Fatal("expected escaping symlink target to be rejected")
	}
}

func TestSanitizeFilename(t *testing.T) {
	got := sanitizeFilename("../../My App (1).ipa")
	if got != "My_App__1_.ipa" {
		t.Fatalf("unexpected sanitized name %q", got)
	}
}

func TestBearerAuthAcceptsAnyConfiguredToken(t *testing.T) {
	handler := (&Server{}).bearerAuth("first", "second")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	for _, token := range []string{"first", "second"} {
		request := httptest.NewRequest(http.MethodGet, "/v1/builds", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("token %q returned status %d", token, response.Code)
		}
	}

	request := httptest.NewRequest(http.MethodGet, "/v1/builds", nil)
	request.Header.Set("Authorization", "Bearer invalid")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token returned status %d", response.Code)
	}
}

func TestDeviceHelpers(t *testing.T) {
	if !deviceUDIDPattern.MatchString("00008120-0012591A0C82201E") {
		t.Fatal("modern UDID was rejected")
	}
	if !deviceUDIDPattern.MatchString("0123456789abcdef0123456789abcdef01234567") {
		t.Fatal("legacy UDID was rejected")
	}
	if deviceUDIDPattern.MatchString("not-a-udid") {
		t.Fatal("invalid UDID was accepted")
	}
	if got := maskUDID("00008120-0012591A0C82201E"); got != "00008120...82201E" {
		t.Fatalf("unexpected masked UDID %q", got)
	}
	if tokenSubject("token-a") == tokenSubject("token-b") {
		t.Fatal("different tokens have the same subject")
	}
}
