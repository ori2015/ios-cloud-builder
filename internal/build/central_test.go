package build

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/MobAI-App/ios-builder/internal/config"
)

func TestCentralDispatchInputs(t *testing.T) {
	cfg := &config.Config{
		ProjectID:         "p_0123456789abcdef0123456789abcdef",
		SnapshotNamespace: "11111111111111111111111111111111",
		Security:          config.SecurityConfig{Recipient: "age1example"},
	}
	want := map[string]string{
		"build_id":           "0192f819-2c07-7c9d-a9ba-0242ac120002",
		"project_id":         "p_0123456789abcdef0123456789abcdef",
		"artifact_recipient": "age1example",
		"operation":          "build",
	}
	if got := centralDispatchInputs(cfg, want["build_id"], false, false); !reflect.DeepEqual(got, want) {
		t.Fatalf("centralDispatchInputs() = %#v, want %#v", got, want)
	}
}

func TestParseEncryptedArtifactExactMembers(t *testing.T) {
	data := makeZIP(t, map[string][]byte{
		centralIPAFile: []byte("age-encrypted-ipa"),
		centralLogFile: []byte("age-encrypted-log"),
	})
	got, err := parseEncryptedArtifact(data)
	if err != nil {
		t.Fatalf("parseEncryptedArtifact() error = %v", err)
	}
	if string(got.ipa) != "age-encrypted-ipa" || string(got.log) != "age-encrypted-log" {
		t.Fatalf("parseEncryptedArtifact() = %#v", got)
	}
	logOnly, err := parseEncryptedArtifact(makeZIP(t, map[string][]byte{centralLogFile: []byte("failure-log-ciphertext")}))
	if err != nil || len(logOnly.ipa) != 0 || string(logOnly.log) != "failure-log-ciphertext" {
		t.Fatalf("parseEncryptedArtifact(log only) = %#v, %v", logOnly, err)
	}

	for name, members := range map[string]map[string][]byte{
		"plaintext IPA": {"App.ipa": []byte("plaintext"), centralLogFile: []byte("ciphertext")},
		"source file":   {"Sources/App.swift": []byte("private"), centralLogFile: []byte("ciphertext")},
		"missing log":   {centralIPAFile: []byte("ciphertext")},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseEncryptedArtifact(makeZIP(t, members)); err == nil {
				t.Fatal("parseEncryptedArtifact() accepted invalid members")
			}
		})
	}
}

func TestVerifyArtifactDigest(t *testing.T) {
	data := []byte("artifact zip bytes")
	sum := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if err := verifyArtifactDigest(digest, data); err != nil {
		t.Fatalf("verifyArtifactDigest(valid) error = %v", err)
	}
	mismatchSuffix := "0"
	if strings.HasSuffix(digest, mismatchSuffix) {
		mismatchSuffix = "1"
	}
	for _, invalid := range []string{"sha512:" + hex.EncodeToString(sum[:]), "sha256:not-hex", digest[:len(digest)-1] + mismatchSuffix} {
		if err := verifyArtifactDigest(invalid, data); err == nil {
			t.Fatalf("verifyArtifactDigest(%q) accepted invalid digest", invalid)
		}
	}
}

func TestDecryptBoundedRejectsOversizePlaintext(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	var ciphertext bytes.Buffer
	w, err := age.Encrypt(&ciphertext, identity.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("123456")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := decryptBounded(identity, ciphertext.Bytes(), 5); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("decryptBounded() error = %v", err)
	}
}

func TestCentralDispatchInputsKeepRepositoriesIndependent(t *testing.T) {
	first := &config.Config{
		ProjectID: "p_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Security:  config.SecurityConfig{Recipient: "age1recipient-a"},
	}
	second := &config.Config{
		ProjectID: "p_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Security:  config.SecurityConfig{Recipient: "age1recipient-b"},
	}
	a := centralDispatchInputs(first, "550e8400-e29b-41d4-a716-446655440000", false, false)
	b := centralDispatchInputs(second, "6ba7b810-9dad-41d1-80b4-00c04fd430c8", false, false)
	if a["project_id"] == b["project_id"] || a["build_id"] == b["build_id"] {
		t.Fatalf("central dispatches were not isolated: %#v %#v", a, b)
	}
	for _, payload := range []map[string]string{a, b} {
		for key := range payload {
			if strings.Contains(strings.ToLower(key), "token") || strings.Contains(strings.ToLower(key), "private_key") {
				t.Fatalf("local or runner credential leaked into dispatch payload key %q", key)
			}
		}
	}
}

func TestCentralTestFlightDispatchForcesRelease(t *testing.T) {
	cfg := &config.Config{
		ProjectID: "p_0123456789abcdef0123456789abcdef",
		Security:  config.SecurityConfig{Recipient: "age1recipient"},
	}
	got := centralDispatchInputs(cfg, "id", true, false)
	if got["operation"] != "testflight" || len(got) != 4 {
		t.Fatalf("TestFlight dispatch = %#v", got)
	}
}

func TestCentralAdHocDispatchSelectsAdHocOperation(t *testing.T) {
	cfg := &config.Config{
		ProjectID: "p_0123456789abcdef0123456789abcdef",
		Security:  config.SecurityConfig{Recipient: "age1recipient"},
	}
	got := centralDispatchInputs(cfg, "id", false, true)
	if got["operation"] != "adhoc" || len(got) != 4 {
		t.Fatalf("ad hoc dispatch = %#v", got)
	}
	// TestFlight is the stronger request and must win if both are somehow set,
	// so an ad hoc flag can never downgrade a deployment into a returned IPA.
	if both := centralDispatchInputs(cfg, "id", true, true); both["operation"] != "testflight" {
		t.Fatalf("combined dispatch = %#v", both)
	}
}

func TestValidateIPA(t *testing.T) {
	valid := makeZIP(t, map[string][]byte{
		"Payload/Test.app/Info.plist": []byte(`<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict><key>CFBundleExecutable</key><string>Test</string></dict></plist>`),
		"Payload/Test.app/Test": []byte("Mach-O"),
	})
	if err := validateIPA(valid); err != nil {
		t.Fatalf("validateIPA(valid) error = %v", err)
	}

	missingExecutable := makeZIP(t, map[string][]byte{
		"Payload/Test.app/Info.plist": []byte(`<?xml version="1.0"?><plist version="1.0"><dict><key>CFBundleExecutable</key><string>Test</string></dict></plist>`),
	})
	if err := validateIPA(missingExecutable); err == nil || !strings.Contains(err.Error(), "executable") {
		t.Fatalf("validateIPA(missing executable) error = %v", err)
	}

	multipleApps := makeZIP(t, map[string][]byte{
		"Payload/One.app/Info.plist": []byte("plist"),
		"Payload/Two.app/Info.plist": []byte("plist"),
	})
	if err := validateIPA(multipleApps); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("validateIPA(multiple apps) error = %v", err)
	}
}

func TestAtomicWritePrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "result.log")
	if err := atomicWritePrivate(path, []byte("private diagnostics")); err != nil {
		t.Fatalf("atomicWritePrivate() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "private diagnostics" {
		t.Fatalf("contents = %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %v; want 0600", info.Mode().Perm())
	}
}

func TestCentralOutputNameCannotEscapeDirectory(t *testing.T) {
	got := centralOutputName("../../Private App", "", ".ipa")
	if strings.ContainsAny(got, `/\\`) || strings.HasPrefix(got, ".") {
		t.Fatalf("centralOutputName() = %q", got)
	}
	if got != "Private-App.ipa" {
		t.Fatalf("centralOutputName() = %q, want Private-App.ipa", got)
	}
}

func TestDownloadLogsRequiresCanonicalUUIDv4(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Project:           "App",
		ProjectID:         "p_0123456789abcdef0123456789abcdef",
		SnapshotNamespace: "11111111111111111111111111111111",
		Backend:           config.BackendCentral,
		GitHub:            config.GitHubConfig{Owner: "source", Repo: "private"},
		Builder:           config.BuilderConfig{Owner: "builder", Repo: "public", Workflow: "ios-build.yml"},
		Security:          config.SecurityConfig{Recipient: identity.Recipient().String()},
	}
	coordinator := NewCoordinatorWithOutput(cfg, nil, io.Discard)
	for _, buildID := range []string{
		"0192f819-2c07-7c9d-a9ba-0242ac120002", // UUIDv7
		"550E8400-E29B-41D4-A716-446655440000", // non-canonical uppercase UUIDv4
	} {
		if _, err := coordinator.DownloadLogs(context.Background(), buildID, t.TempDir()); err == nil || !strings.Contains(err.Error(), "UUIDv4") {
			t.Fatalf("DownloadLogs(%q) error = %v", buildID, err)
		}
	}
}

func makeZIP(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	zw := zip.NewWriter(&buffer)
	for name, data := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
