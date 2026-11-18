package worker

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func (a *Agent) claimDevice(ctx context.Context) (*Device, error) {
	ctx, cancel := context.WithTimeout(ctx, a.requestTimeout())
	defer cancel()
	body, _ := json.Marshal(map[string]string{"worker_id": a.cfg.WorkerID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.ServerURL+"/v1/agent/devices/claim", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.AgentToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("claim device returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var device Device
	if err := json.NewDecoder(resp.Body).Decode(&device); err != nil {
		return nil, err
	}
	return &device, nil
}

func (a *Agent) processDevice(ctx context.Context, device Device) {
	log.Printf("registering Apple device %s (%s)", maskDeviceUDID(device.UDID), device.Name)
	appleID, err := a.registerAppleDevice(ctx, device)
	if err != nil {
		log.Printf("device %s registration failed: %v", maskDeviceUDID(device.UDID), err)
		if completeErr := a.completeDevice(ctx, device.ID, "failed", "", err.Error()); completeErr != nil {
			log.Printf("report device registration failure: %v", completeErr)
		}
		return
	}
	if err := a.completeDevice(ctx, device.ID, "registered", appleID, ""); err != nil {
		log.Printf("device %s registered with Apple but reporting completion failed: %v", maskDeviceUDID(device.UDID), err)
		return
	}
	log.Printf("device %s registered", maskDeviceUDID(device.UDID))
}

func (a *Agent) completeDevice(ctx context.Context, id, status, appleID, errorMessage string) error {
	ctx, cancel := context.WithTimeout(ctx, a.requestTimeout())
	defer cancel()
	body, _ := json.Marshal(map[string]string{
		"status": status, "apple_device_id": appleID, "error": errorMessage,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.ServerURL+"/v1/agent/devices/"+id+"/complete", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.AgentToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("complete device returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return nil
}

func (a *Agent) registerAppleDevice(ctx context.Context, device Device) (string, error) {
	if a.cfg.APIKeyPath == "" || a.cfg.APIKeyID == "" || a.cfg.APIIssuerID == "" {
		return "", fmt.Errorf("APPLE_API_KEY_PATH, APPLE_API_KEY_ID and APPLE_API_ISSUER_ID are required for device enrollment")
	}
	token, err := a.appleAPIToken(time.Now())
	if err != nil {
		return "", err
	}
	payload := map[string]any{
		"data": map[string]any{
			"type": "devices",
			"attributes": map[string]string{
				"name": device.Name, "platform": device.Platform, "udid": device.UDID,
			},
		},
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.AppleAPIBaseURL+"/v1/devices", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("register Apple device: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		return a.findAppleDevice(ctx, token, device.UDID)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		message := strings.ReplaceAll(strings.TrimSpace(string(data)), device.UDID, maskDeviceUDID(device.UDID))
		return "", fmt.Errorf("Apple device registration returned %s: %s", resp.Status, message)
	}
	return decodeAppleDeviceID(resp.Body)
}

func (a *Agent) findAppleDevice(ctx context.Context, token, udid string) (string, error) {
	endpoint := a.cfg.AppleAPIBaseURL + "/v1/devices?filter%5Budid%5D=" + url.QueryEscape(udid) + "&limit=1"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := a.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("find existing Apple device: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("find existing Apple device returned %s", resp.Status)
	}
	var result struct {
		Data []struct {
			ID         string `json:"id"`
			Attributes struct {
				Status string `json:"status"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if len(result.Data) == 0 || result.Data[0].ID == "" {
		return "", fmt.Errorf("Apple reported a duplicate device but it could not be found")
	}
	if status := strings.ToUpper(result.Data[0].Attributes.Status); status != "" && status != "ENABLED" {
		return "", fmt.Errorf("the existing Apple device is %s and cannot be used for Ad Hoc distribution", status)
	}
	return result.Data[0].ID, nil
}

func decodeAppleDeviceID(reader io.Reader) (string, error) {
	var result struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(reader).Decode(&result); err != nil {
		return "", err
	}
	if result.Data.ID == "" {
		return "", fmt.Errorf("Apple device response did not contain an id")
	}
	return result.Data.ID, nil
}

func (a *Agent) appleAPIToken(now time.Time) (string, error) {
	keyData, err := os.ReadFile(a.cfg.APIKeyPath)
	if err != nil {
		return "", fmt.Errorf("read Apple API key: %w", err)
	}
	block, _ := pem.Decode(keyData)
	if block == nil {
		return "", fmt.Errorf("Apple API key is not valid PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse Apple API key: %w", err)
	}
	privateKey, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return "", fmt.Errorf("Apple API key is not an EC private key")
	}
	header, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": a.cfg.APIKeyID, "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{
		"iss": a.cfg.APIIssuerID,
		"iat": now.Unix(),
		"exp": now.Add(15 * time.Minute).Unix(),
		"aud": "appstoreconnect-v1",
	})
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(unsigned))
	r, s, err := ecdsa.Sign(rand.Reader, privateKey, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign Apple API token: %w", err)
	}
	signature := append(paddedBytes(r, 32), paddedBytes(s, 32)...)
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func paddedBytes(value *big.Int, size int) []byte {
	result := make([]byte, size)
	bytes := value.Bytes()
	copy(result[size-len(bytes):], bytes)
	return result
}

func maskDeviceUDID(udid string) string {
	if len(udid) <= 12 {
		return "***"
	}
	return udid[:8] + "..." + udid[len(udid)-6:]
}
