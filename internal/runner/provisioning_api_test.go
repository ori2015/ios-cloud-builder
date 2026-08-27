package runner

import (
	"context"
	"crypto/sha1" // #nosec G505 -- mirrors Apple's signing certificate fingerprint.
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDownloadASCProvisioningProfilesUsesExactBundle(t *testing.T) {
	profile := base64.StdEncoding.EncodeToString([]byte("mobile profile"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/bundleIds":
			if r.URL.Query().Get("filter[identifier]") != "com.example.app" || r.URL.Query().Has("filter[platform]") {
				t.Fatalf("unexpected bundle query: %s", r.URL.RawQuery)
			}
			writeASCJSON(w, `{"data":[{"type":"bundleIds","id":"bundle1","attributes":{"identifier":"com.example.app","platform":"UNIVERSAL"}}]}`)
		case "/v1/bundleIds/bundle1/profiles":
			writeASCJSON(w, `{"data":[`+
				`{"type":"profiles","id":"profile1","attributes":{"name":"active","profileType":"IOS_APP_STORE","profileState":"ACTIVE","profileContent":"`+profile+`"}},`+
				`{"type":"profiles","id":"profile2","attributes":{"name":"development","profileType":"IOS_APP_DEVELOPMENT","profileState":"ACTIVE","profileContent":"`+profile+`"}}]}`)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	client, _ := testASCClient(t, server.Client())
	client.baseURL = server.URL
	destination := t.TempDir()
	bundleResourceID, paths, err := downloadASCProvisioningProfiles(context.Background(), client, "com.example.app", destination, ascAppStoreProfileType)
	if err != nil {
		t.Fatal(err)
	}
	if bundleResourceID != "bundle1" || len(paths) != 1 {
		t.Fatalf("bundle = %q, paths = %#v", bundleResourceID, paths)
	}
	contents, err := os.ReadFile(paths[0])
	if err != nil || string(contents) != "mobile profile" {
		t.Fatalf("profile contents = %q, %v", contents, err)
	}
	if filepath.Dir(paths[0]) != destination {
		t.Fatalf("profile escaped destination: %s", paths[0])
	}
}

func TestCreateASCProvisioningProfileBindsExactCertificate(t *testing.T) {
	certificate := []byte("distribution certificate DER")
	fingerprint := sha1.Sum(certificate) // #nosec G401 -- mirrors Apple's signing identity fingerprint.
	identity := strings.ToUpper(hex.EncodeToString(fingerprint[:]))
	profile := base64.StdEncoding.EncodeToString([]byte("created mobile profile"))
	var createBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/certificates":
			if r.URL.Query().Get("filter[certificateType]") != "DISTRIBUTION,IOS_DISTRIBUTION" {
				t.Fatalf("unexpected certificate query: %s", r.URL.RawQuery)
			}
			if strings.Contains(r.URL.Query().Get("fields[certificates]"), "activated") {
				t.Fatalf("requested unsupported certificate field: %s", r.URL.RawQuery)
			}
			attributes, _ := json.Marshal(map[string]any{
				"certificateType": "DISTRIBUTION", "certificateContent": base64.StdEncoding.EncodeToString(certificate),
				"expirationDate": time.Now().Add(24 * time.Hour).UTC(),
			})
			writeASCJSON(w, `{"data":[{"type":"certificates","id":"cert1","attributes":`+string(attributes)+`}]}`)
		case "/v1/profiles":
			if r.Method != http.MethodPost {
				t.Fatalf("unexpected method: %s", r.Method)
			}
			if err := json.NewDecoder(r.Body).Decode(&createBody); err != nil {
				t.Fatal(err)
			}
			w.WriteHeader(http.StatusCreated)
			writeASCJSON(w, `{"data":{"type":"profiles","id":"profile1","attributes":{"name":"generated","profileType":"IOS_APP_STORE","profileState":"ACTIVE","profileContent":"`+profile+`"}}}`)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	client, _ := testASCClient(t, server.Client())
	client.baseURL = server.URL
	profilePath, err := createASCProvisioningProfile(context.Background(), client, "bundle1", "com.example.app", identity, t.TempDir(), ascAppStoreProfileType)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(profilePath)
	if err != nil || string(contents) != "created mobile profile" {
		t.Fatalf("created profile = %q, %v", contents, err)
	}
	encoded, _ := json.Marshal(createBody)
	body := string(encoded)
	for _, required := range []string{`"profileType":"IOS_APP_STORE"`, `"id":"bundle1"`, `"id":"cert1"`} {
		if !strings.Contains(body, required) {
			t.Fatalf("create body %s does not contain %s", body, required)
		}
	}
}

func TestFindASCCertificateRejectsDifferentIdentity(t *testing.T) {
	attributes, _ := json.Marshal(map[string]any{
		"certificateType": "DISTRIBUTION", "certificateContent": base64.StdEncoding.EncodeToString([]byte("other certificate")),
		"expirationDate": time.Now().Add(24 * time.Hour).UTC(),
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeASCJSON(w, `{"data":[{"type":"certificates","id":"cert1","attributes":`+string(attributes)+`}]}`)
	}))
	defer server.Close()
	client, _ := testASCClient(t, server.Client())
	client.baseURL = server.URL
	if _, err := findASCCertificate(context.Background(), client, strings.Repeat("A", 40)); err == nil {
		t.Fatal("different signing certificate was accepted")
	}
}

func TestFindASCCertificateRejectsNonDistributionType(t *testing.T) {
	certificate := []byte("development certificate")
	fingerprint := sha1.Sum(certificate) // #nosec G401 -- mirrors Apple's signing identity fingerprint.
	attributes, _ := json.Marshal(map[string]any{
		"certificateType": "DEVELOPMENT", "certificateContent": base64.StdEncoding.EncodeToString(certificate),
		"expirationDate": time.Now().Add(24 * time.Hour).UTC(),
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeASCJSON(w, `{"data":[{"type":"certificates","id":"cert1","attributes":`+string(attributes)+`}]}`)
	}))
	defer server.Close()
	client, _ := testASCClient(t, server.Client())
	client.baseURL = server.URL
	if _, err := findASCCertificate(context.Background(), client, hex.EncodeToString(fingerprint[:])); err == nil {
		t.Fatal("non-distribution signing certificate was accepted")
	}
}
