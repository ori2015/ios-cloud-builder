package runner

import (
	"context"
	"crypto/rand"
	"crypto/sha1" // #nosec G505 -- Apple identifies signing certificates by SHA-1 fingerprint.
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

const ascAppStoreProfileType = "IOS_APP_STORE"

type ascProfileAttributes struct {
	Name           string `json:"name"`
	ProfileType    string `json:"profileType"`
	ProfileState   string `json:"profileState"`
	ProfileContent string `json:"profileContent"`
}

func downloadASCProvisioningProfiles(ctx context.Context, api *appStoreConnectClient, bundleID, destinationDir string) (string, []string, error) {
	if api == nil || !bundleIDPattern.MatchString(bundleID) {
		return "", nil, fmt.Errorf("invalid App Store Connect provisioning metadata")
	}
	bundleResourceID, err := findASCBundleID(ctx, api, bundleID)
	if err != nil {
		return "", nil, err
	}
	query := url.Values{
		"fields[profiles]": {"name,profileType,profileState,profileContent"},
		"limit":            {"200"},
	}
	var response ascListResponse
	apiPath := "/v1/bundleIds/" + url.PathEscape(bundleResourceID) + "/profiles"
	if err := api.request(ctx, http.MethodGet, apiPath, query, nil, &response); err != nil {
		return "", nil, err
	}
	paths := make([]string, 0, len(response.Data))
	for index, resource := range response.Data {
		var attributes ascProfileAttributes
		if resource.Type != "profiles" || json.Unmarshal(resource.Attributes, &attributes) != nil {
			return "", nil, fmt.Errorf("parse App Store Connect provisioning profile")
		}
		if attributes.ProfileType != ascAppStoreProfileType || attributes.ProfileState != "ACTIVE" {
			continue
		}
		profilePath := filepath.Join(destinationDir, fmt.Sprintf("profile-asc-%03d.mobileprovision", index))
		if err := writeASCProfileContent(attributes.ProfileContent, profilePath); err != nil {
			return "", nil, err
		}
		paths = append(paths, profilePath)
	}
	return bundleResourceID, paths, nil
}

func findASCBundleID(ctx context.Context, api *appStoreConnectClient, bundleID string) (string, error) {
	query := url.Values{
		"filter[identifier]": {bundleID},
		"filter[platform]":   {"IOS"},
		"fields[bundleIds]":  {"identifier,platform"},
		"limit":              {"200"},
	}
	var response ascListResponse
	if err := api.request(ctx, http.MethodGet, "/v1/bundleIds", query, nil, &response); err != nil {
		return "", err
	}
	var exact string
	for _, resource := range response.Data {
		var attributes struct {
			Identifier string `json:"identifier"`
			Platform   string `json:"platform"`
		}
		if resource.Type != "bundleIds" || json.Unmarshal(resource.Attributes, &attributes) != nil {
			return "", fmt.Errorf("parse App Store Connect bundle identifier")
		}
		if attributes.Identifier == bundleID && attributes.Platform == "IOS" {
			if exact != "" {
				return "", fmt.Errorf("multiple App Store Connect bundle identifiers matched exactly")
			}
			exact = resource.ID
		}
	}
	if exact == "" {
		return "", fmt.Errorf("no exact iOS bundle identifier exists in App Store Connect")
	}
	return exact, nil
}

func createASCProvisioningProfile(ctx context.Context, api *appStoreConnectClient, bundleResourceID, bundleID, identityFingerprint, destinationDir string) (string, error) {
	if api == nil || bundleResourceID == "" || !bundleIDPattern.MatchString(bundleID) || len(identityFingerprint) != 40 {
		return "", fmt.Errorf("invalid App Store Connect provisioning metadata")
	}
	certificateID, err := findASCCertificate(ctx, api, identityFingerprint)
	if err != nil {
		return "", err
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return "", fmt.Errorf("generate provisioning profile name")
	}
	name := "ios-cloud-builder " + bundleID + " " + hex.EncodeToString(suffix)
	if len(name) > 100 {
		name = name[:91] + " " + hex.EncodeToString(suffix)
	}
	body := map[string]any{"data": map[string]any{
		"type": "profiles",
		"attributes": map[string]any{
			"name":        name,
			"profileType": ascAppStoreProfileType,
		},
		"relationships": map[string]any{
			"bundleId":     map[string]any{"data": map[string]string{"type": "bundleIds", "id": bundleResourceID}},
			"certificates": map[string]any{"data": []map[string]string{{"type": "certificates", "id": certificateID}}},
		},
	}}
	var response ascSingleResponse
	if err := api.request(ctx, http.MethodPost, "/v1/profiles", nil, body, &response); err != nil {
		return "", err
	}
	if response.Data == nil || response.Data.Type != "profiles" {
		return "", fmt.Errorf("no created provisioning profile returned by App Store Connect")
	}
	var attributes ascProfileAttributes
	if json.Unmarshal(response.Data.Attributes, &attributes) != nil || attributes.ProfileType != ascAppStoreProfileType {
		return "", fmt.Errorf("parse created App Store Connect provisioning profile")
	}
	profilePath := filepath.Join(destinationDir, "profile-asc-created.mobileprovision")
	if err := writeASCProfileContent(attributes.ProfileContent, profilePath); err != nil {
		return "", err
	}
	return profilePath, nil
}

func findASCCertificate(ctx context.Context, api *appStoreConnectClient, identityFingerprint string) (string, error) {
	query := url.Values{
		"fields[certificates]":    {"certificateType,certificateContent,expirationDate,activated"},
		"filter[certificateType]": {"DISTRIBUTION,IOS_DISTRIBUTION"},
		"limit":                   {"200"},
	}
	var response ascListResponse
	if err := api.request(ctx, http.MethodGet, "/v1/certificates", query, nil, &response); err != nil {
		return "", err
	}
	for _, resource := range response.Data {
		var attributes struct {
			CertificateType    string    `json:"certificateType"`
			CertificateContent string    `json:"certificateContent"`
			ExpirationDate     time.Time `json:"expirationDate"`
			Activated          bool      `json:"activated"`
		}
		if resource.Type != "certificates" || json.Unmarshal(resource.Attributes, &attributes) != nil {
			return "", fmt.Errorf("parse App Store Connect signing certificate")
		}
		if attributes.CertificateType != "DISTRIBUTION" && attributes.CertificateType != "IOS_DISTRIBUTION" {
			continue
		}
		certificate, err := decodeASCBase64(attributes.CertificateContent, maxProfileBytes)
		if err != nil {
			return "", fmt.Errorf("decode App Store Connect signing certificate")
		}
		fingerprint := sha1.Sum(certificate) // #nosec G401 -- required to match Apple's signing identity.
		if strings.EqualFold(hex.EncodeToString(fingerprint[:]), identityFingerprint) && attributes.Activated &&
			attributes.ExpirationDate.After(time.Now().Add(5*time.Minute)) {
			return resource.ID, nil
		}
	}
	return "", fmt.Errorf("imported distribution certificate not found in App Store Connect")
}

func writeASCProfileContent(value, destination string) error {
	data, err := decodeASCBase64(value, maxProfileBytes)
	if err != nil {
		return fmt.Errorf("decode App Store Connect provisioning profile")
	}
	return writePrivateFile(destination, data)
}

func decodeASCBase64(value string, limit int64) ([]byte, error) {
	compact := strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, value)
	if compact == "" || int64(len(compact)) > limit*2 {
		return nil, fmt.Errorf("invalid base64 payload")
	}
	data, err := base64.StdEncoding.DecodeString(compact)
	if err != nil || len(data) == 0 || int64(len(data)) > limit {
		return nil, fmt.Errorf("invalid base64 payload")
	}
	return data, nil
}
