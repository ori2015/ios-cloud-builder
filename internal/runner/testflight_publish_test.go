package runner

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

const testGroupID = "87331d26-d22c-4c6f-a855-081491d718b2"

func TestBetaGroupForBundleUsesExactValidatedMapping(t *testing.T) {
	raw := `{"com.example.app":{"group_id":"` + testGroupID + `","public_link_id":"abcd1234","submit_beta_review":true}}`
	group, err := betaGroupForBundle(raw, "com.example.app")
	if err != nil || group == nil || group.GroupID != testGroupID || !group.SubmitBetaReview {
		t.Fatalf("betaGroupForBundle() = %#v, %v", group, err)
	}
	if group, err := betaGroupForBundle(raw, "com.example.other"); err != nil || group != nil {
		t.Fatalf("unconfigured bundle = %#v, %v", group, err)
	}
	for _, invalid := range []string{
		`{"com.example.app":{"group_id":"bad","public_link_id":"abcd1234"}}`,
		`{"com.example.app":{"group_id":"` + testGroupID + `","public_link_id":"bad link"}}`,
		`{"com.example.app":{"group_id":"` + testGroupID + `","public_link_id":"abcd1234","extra":true}}`,
	} {
		if _, err := betaGroupForBundle(invalid, "com.example.app"); err == nil {
			t.Fatalf("invalid mapping accepted: %s", invalid)
		}
	}
}

func TestAppStoreConnectTokenIsValidES256JWT(t *testing.T) {
	client, key := testASCClient(t, nil)
	client.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	token, err := client.token()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT parts = %d", len(parts))
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(signature) != 64 {
		t.Fatalf("signature length = %d, %v", len(signature), err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(signature[:32])
	s := new(big.Int).SetBytes(signature[32:])
	if !ecdsa.Verify(&key.PublicKey, digest[:], r, s) {
		t.Fatal("JWT signature did not verify")
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if !bytes.Contains(payload, []byte(`"aud":"appstoreconnect-v1"`)) || !bytes.Contains(payload, []byte(`"iss":"issuer"`)) {
		t.Fatalf("unexpected JWT payload: %s", payload)
	}
}

func TestPublishToBetaGroupWaitsAttachesAndSubmits(t *testing.T) {
	var mutations []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Fatalf("missing bearer authorization on %s", r.URL.Path)
		}
		key := r.Method + " " + r.URL.Path
		switch key {
		case "GET /v1/apps":
			writeASCJSON(w, `{"data":[{"type":"apps","id":"app1","attributes":{"bundleId":"com.example.app"}}]}`)
		case "GET /v1/apps/app1/betaGroups":
			writeASCJSON(w, `{"data":[{"type":"betaGroups","id":"`+testGroupID+`","attributes":{"name":"anyone","publicLinkId":"abcd1234","publicLink":"https://testflight.apple.com/join/abcd1234","isInternalGroup":false}}]}`)
		case "GET /v1/builds":
			if r.URL.Query().Get("filter[app]") != "app1" || r.URL.Query().Get("filter[version]") != "16.1" ||
				r.URL.Query().Get("filter[preReleaseVersion.version]") != "0.1.0" {
				t.Fatalf("unsafe build query: %s", r.URL.RawQuery)
			}
			writeASCJSON(w, `{"data":[{"type":"builds","id":"build1","attributes":{"processingState":"VALID","usesNonExemptEncryption":false}}]}`)
		case "GET /v1/builds/build1/betaBuildLocalizations":
			writeASCJSON(w, `{"data":[{"type":"betaBuildLocalizations","id":"loc1","attributes":{}}]}`)
		case "PATCH /v1/betaBuildLocalizations/loc1":
			mutations = append(mutations, key)
			w.WriteHeader(http.StatusNoContent)
		case "POST /v1/betaGroups/" + testGroupID + "/relationships/builds":
			mutations = append(mutations, key)
			w.WriteHeader(http.StatusNoContent)
		case "GET /v1/apps/app1/betaAppLocalizations":
			writeASCJSON(w, `{"data":[{"type":"betaAppLocalizations","id":"app-loc","attributes":{"description":"test","feedbackEmail":"test@example.com"}}]}`)
		case "GET /v1/apps/app1/betaAppReviewDetail":
			writeASCJSON(w, `{"data":{"type":"betaAppReviewDetails","id":"detail1","attributes":{"contactFirstName":"A","contactLastName":"B","contactPhone":"1","contactEmail":"test@example.com"}}}`)
		case "GET /v1/builds/build1/betaAppReviewSubmission":
			writeASCJSON(w, `{"data":null}`)
		case "POST /v1/betaAppReviewSubmissions":
			mutations = append(mutations, key)
			w.WriteHeader(http.StatusCreated)
		case "GET /v1/builds/build1/buildBetaDetail":
			writeASCJSON(w, `{"data":{"type":"buildBetaDetails","id":"detail","attributes":{"externalBuildState":"READY_FOR_BETA_TESTING"}}}`)
		case "GET /v1/betaGroups/" + testGroupID:
			writeASCJSON(w, `{"data":{"type":"betaGroups","id":"`+testGroupID+`","attributes":{"name":"anyone","publicLinkId":"abcd1234","publicLink":"https://testflight.apple.com/join/abcd1234","isInternalGroup":false}}}`)
		default:
			t.Fatalf("unexpected request %s", key)
		}
	}))
	defer server.Close()
	client, _ := testASCClient(t, server.Client())
	client.baseURL = server.URL
	var privateLog bytes.Buffer
	err := publishToBetaGroup(context.Background(), client, &betaPublishRequest{
		BundleID: "com.example.app", MarketingVersion: "0.1.0", BuildNumber: "16.1",
		Group: betaGroupConfig{GroupID: testGroupID, PublicLinkID: "abcd1234", SubmitBetaReview: true},
	}, &privateLog)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"PATCH /v1/betaBuildLocalizations/loc1",
		"POST /v1/betaGroups/" + testGroupID + "/relationships/builds",
		"POST /v1/betaAppReviewSubmissions",
	}
	if !reflect.DeepEqual(mutations, want) {
		t.Fatalf("mutations = %#v, want %#v", mutations, want)
	}
	if !strings.Contains(privateLog.String(), "finished processing") || !strings.Contains(privateLog.String(), "attached") {
		t.Fatalf("private log = %q", privateLog.String())
	}
}

func TestNextASCBuildNumberContinuesExistingIntegerSequence(t *testing.T) {
	var serverURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/apps":
			if r.URL.Query().Get("filter[bundleId]") != "com.example.app" {
				t.Fatalf("unexpected app query: %s", r.URL.RawQuery)
			}
			writeASCJSON(w, `{"data":[{"type":"apps","id":"app1","attributes":{"bundleId":"com.example.app"}}]}`)
		case "/v1/builds":
			if r.URL.Query().Get("filter[app]") != "app1" ||
				r.URL.Query().Get("filter[preReleaseVersion.version]") != "0.1.0" {
				t.Fatalf("unexpected build query: %s", r.URL.RawQuery)
			}
			if r.URL.Query().Get("cursor") == "second" {
				writeASCJSON(w, `{"data":[{"type":"builds","id":"b3","attributes":{"version":"17.1"}}]}`)
				return
			}
			writeASCJSON(w, `{"data":[`+
				`{"type":"builds","id":"b1","attributes":{"version":"36"}},`+
				`{"type":"builds","id":"b2","attributes":{"version":"16.1"}}],`+
				`"links":{"next":"`+serverURL+`/v1/builds?cursor=second&filter%5Bapp%5D=app1&filter%5BpreReleaseVersion.version%5D=0.1.0"}}`)
		default:
			t.Fatalf("unexpected request path: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	serverURL = server.URL
	client, _ := testASCClient(t, server.Client())
	client.baseURL = server.URL

	number, err := nextASCBuildNumber(t.Context(), client, "com.example.app", "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if number != "37" {
		t.Fatalf("next build number = %q, want 37", number)
	}
}

func TestNextASCBuildNumberStartsAtOne(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/apps":
			writeASCJSON(w, `{"data":[{"type":"apps","id":"app1","attributes":{"bundleId":"com.example.app"}}]}`)
		case "/v1/builds":
			writeASCJSON(w, `{"data":[]}`)
		default:
			t.Fatalf("unexpected request path: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	client, _ := testASCClient(t, server.Client())
	client.baseURL = server.URL

	number, err := nextASCBuildNumber(t.Context(), client, "com.example.app", "0.1.0")
	if err != nil || number != "1" {
		t.Fatalf("next build number = %q, %v", number, err)
	}
}

func testASCClient(t *testing.T, httpClient *http.Client) (*appStoreConnectClient, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pemValue := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	client, err := newAppStoreConnectClient("KEY1234567", "issuer", string(pemValue))
	if err != nil {
		t.Fatal(err)
	}
	if httpClient != nil {
		client.httpClient = httpClient
	}
	return client, key
}

func writeASCJSON(w http.ResponseWriter, value string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(value))
}
