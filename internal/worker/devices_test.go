package worker

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRegisterAppleDevice(t *testing.T) {
	privateKey := writeTestAppleKey(t)
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/devices" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		authorization = r.Header.Get("Authorization")
		var payload struct {
			Data struct {
				Attributes struct {
					UDID string `json:"udid"`
				} `json:"attributes"`
			} `json:"data"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Data.Attributes.UDID != "00008120-0012591A0C82201E" {
			t.Fatalf("unexpected UDID %q", payload.Data.Attributes.UDID)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"data":{"type":"devices","id":"APPLE-DEVICE-ID"}}`))
	}))
	defer server.Close()

	agent := New(Config{
		APIKeyPath:      privateKey.path,
		APIKeyID:        "KEY123",
		APIIssuerID:     "ISSUER123",
		AppleAPIBaseURL: server.URL,
	})
	id, err := agent.registerAppleDevice(context.Background(), Device{
		UDID: "00008120-0012591A0C82201E", Name: "Test iPhone", Platform: "IOS",
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "APPLE-DEVICE-ID" {
		t.Fatalf("unexpected Apple device ID %q", id)
	}
	verifyTestJWT(t, strings.TrimPrefix(authorization, "Bearer "), privateKey.key)
}

func TestRegisterAppleDeviceTreatsDuplicateAsExisting(t *testing.T) {
	privateKey := writeTestAppleKey(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusConflict)
			return
		}
		if r.Method != http.MethodGet || r.URL.Query().Get("filter[udid]") == "" {
			t.Fatalf("unexpected duplicate lookup %s", r.URL.String())
		}
		_, _ = w.Write([]byte(`{"data":[{"type":"devices","id":"EXISTING-ID"}]}`))
	}))
	defer server.Close()
	agent := New(Config{APIKeyPath: privateKey.path, APIKeyID: "KEY", APIIssuerID: "ISSUER", AppleAPIBaseURL: server.URL})
	id, err := agent.registerAppleDevice(context.Background(), Device{UDID: "00008120-0012591A0C82201E", Name: "Test", Platform: "IOS"})
	if err != nil {
		t.Fatal(err)
	}
	if id != "EXISTING-ID" {
		t.Fatalf("unexpected existing ID %q", id)
	}
}

type testAppleKey struct {
	path string
	key  *ecdsa.PrivateKey
}

func writeTestAppleKey(t *testing.T) testAppleKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "AuthKey_TEST.p8")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}), 0o600); err != nil {
		t.Fatal(err)
	}
	return testAppleKey{path: path, key: key}
}

func verifyTestJWT(t *testing.T, token string, publicKey *ecdsa.PrivateKey) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("invalid JWT parts: %d", len(parts))
	}
	headerBytes, _ := base64.RawURLEncoding.DecodeString(parts[0])
	claimsBytes, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var header map[string]any
	var claims map[string]any
	_ = json.Unmarshal(headerBytes, &header)
	_ = json.Unmarshal(claimsBytes, &claims)
	if header["kid"] != "KEY123" || claims["iss"] != "ISSUER123" || claims["aud"] != "appstoreconnect-v1" {
		t.Fatalf("unexpected JWT header/claims: %#v %#v", header, claims)
	}
	if claims["exp"].(float64)-claims["iat"].(float64) != (15 * time.Minute).Seconds() {
		t.Fatal("unexpected JWT lifetime")
	}
	signature, _ := base64.RawURLEncoding.DecodeString(parts[2])
	if len(signature) != 64 {
		t.Fatalf("unexpected signature size %d", len(signature))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(signature[:32])
	s := new(big.Int).SetBytes(signature[32:])
	if !ecdsa.Verify(&publicKey.PublicKey, digest[:], r, s) {
		t.Fatal("JWT signature verification failed")
	}
}
