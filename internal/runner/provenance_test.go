package runner

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
)

func provenanceFixture(t *testing.T) (string, ProvenanceExpectation) {
	t.Helper()
	dir := t.TempDir()
	ciphertext := []byte("age-encrypted-exact-ipa")
	if err := os.WriteFile(filepath.Join(dir, trustedIPAFile), ciphertext, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(ciphertext)
	expected := ProvenanceExpectation{
		BuildID: "123e4567-e89b-42d3-a456-426614174000", ProjectID: "p_0123456789abcdef0123456789abcdef",
		BuilderCommit: strings.Repeat("a", 40), WorkflowRef: "owner/repo/.github/workflows/ios-build.yml@refs/heads/main",
	}
	manifest := ProvenanceManifest{
		Version: 1, BuildID: expected.BuildID, ProjectID: expected.ProjectID, Operation: "testflight",
		PlaintextIPASHA256: strings.Repeat("b", 64), CiphertextSHA256: hex.EncodeToString(sum[:]),
		BuilderCommit: expected.BuilderCommit, WorkflowRef: expected.WorkflowRef, CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	writeManifest(t, dir, &manifest)
	return dir, expected
}

func writeManifest(t *testing.T, dir string, manifest *ProvenanceManifest) {
	t.Helper()
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, provenanceFile), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestProvenanceManifestGeneration(t *testing.T) {
	createdAt := time.Date(2026, 8, 25, 12, 0, 0, 0, time.FixedZone("test", 3*60*60))
	options := &TrustedPackageOptions{
		BuildID: "123e4567-e89b-42d3-a456-426614174000", ProjectID: "p_0123456789abcdef0123456789abcdef",
		BuilderCommit: strings.Repeat("a", 40), WorkflowRef: "owner/repo/.github/workflows/ios-build.yml@refs/heads/main",
	}
	manifest, err := newProvenanceManifest(options, strings.Repeat("b", 64), strings.Repeat("c", 64), createdAt)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Version != 1 || manifest.BuildID != options.BuildID || manifest.ProjectID != options.ProjectID ||
		manifest.Operation != "testflight" || manifest.BuilderCommit != options.BuilderCommit || manifest.WorkflowRef != options.WorkflowRef ||
		manifest.PlaintextIPASHA256 != strings.Repeat("b", 64) || manifest.CiphertextSHA256 != strings.Repeat("c", 64) ||
		manifest.CreatedAt != "2026-08-25T09:00:00Z" {
		t.Fatalf("generated manifest = %#v", manifest)
	}
}

func TestTrustedPackageEndToEnd(t *testing.T) {
	packagingIdentity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	signingIdentity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	infoPlist := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>com.example.private</string>
<key>CFBundleShortVersionString</key><string>1.2.3</string>
<key>CFBundleExecutable</key><string>App</string>
</dict></plist>`
	inputIPA := writeDeployTestZIP(t, map[string]zipEntry{
		"Payload/":                   {mode: os.ModeDir | 0755},
		"Payload/App.app/":           {mode: os.ModeDir | 0755},
		"Payload/App.app/Info.plist": {data: infoPlist, mode: 0644},
		"Payload/App.app/App":        {data: "Mach-O fixture", mode: 0755},
	})
	inputDir := filepath.Join(t.TempDir(), "input")
	if err := os.Mkdir(inputDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := encryptAndRemove(packagingIdentity.Recipient(), inputIPA, filepath.Join(inputDir, projectOutputFile), true); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inputDir, "build.log.age"), []byte("encrypted diagnostic fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	outputDir := filepath.Join(t.TempDir(), "trusted-output")
	options := &TrustedPackageOptions{
		InputDir: inputDir, OutputDir: outputDir,
		BuildID: "123e4567-e89b-42d3-a456-426614174000", ProjectID: "p_0123456789abcdef0123456789abcdef",
		BuilderCommit: strings.Repeat("a", 40), WorkflowRef: "owner/repo/.github/workflows/ios-build.yml@refs/heads/main",
	}
	t.Setenv("PACKAGING_AGE_IDENTITY", packagingIdentity.String())
	if err := trustedPackageWithPackager(t.Context(), options, signingIdentity.Recipient().String(), os.Stderr, packageAppFixture); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("PACKAGING_AGE_IDENTITY") != "" {
		t.Fatal("packaging identity remained in the process environment")
	}
	expected := ProvenanceExpectation{
		BuildID: options.BuildID, ProjectID: options.ProjectID,
		BuilderCommit: options.BuilderCommit, WorkflowRef: options.WorkflowRef,
	}
	manifest, err := ValidateProvenanceArtifact(outputDir, expected)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := filepath.Join(t.TempDir(), "App.ipa")
	if err := decryptFileBounded(signingIdentity, filepath.Join(outputDir, trustedIPAFile), plaintext, maxDeployIPABytes); err != nil {
		t.Fatal(err)
	}
	if err := verifyPlaintextIPADigest(plaintext, manifest); err != nil {
		t.Fatal(err)
	}
}

func packageAppFixture(_ executor, appPath, outputPath string) error {
	file, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	writer := zip.NewWriter(file)
	for _, relative := range []string{"Info.plist", "App"} {
		data, readErr := os.ReadFile(filepath.Join(appPath, relative))
		if readErr != nil {
			_ = writer.Close()
			_ = file.Close()
			return readErr
		}
		header := &zip.FileHeader{Name: "Payload/App.app/" + relative, Method: zip.Store}
		if relative == "App" {
			header.SetMode(0755)
		} else {
			header.SetMode(0644)
		}
		member, createErr := writer.CreateHeader(header)
		if createErr != nil {
			_ = writer.Close()
			_ = file.Close()
			return createErr
		}
		if _, writeErr := member.Write(data); writeErr != nil {
			_ = writer.Close()
			_ = file.Close()
			return writeErr
		}
	}
	if err := writer.Close(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func TestTrustedApplicationRejectsMissingExecutable(t *testing.T) {
	app := filepath.Join(t.TempDir(), "App.app")
	if err := os.Mkdir(app, 0700); err != nil {
		t.Fatal(err)
	}
	infoPlist := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>com.example.private</string>
<key>CFBundleShortVersionString</key><string>1.2.3</string>
<key>CFBundleExecutable</key><string>Missing</string>
</dict></plist>`
	if err := os.WriteFile(filepath.Join(app, "Info.plist"), []byte(infoPlist), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateTrustedApplication(app); err == nil {
		t.Fatal("application with a missing executable was accepted")
	}
}

func TestProvenanceManifestVerificationAndArtifactConfusion(t *testing.T) {
	dir, expected := provenanceFixture(t)
	if _, err := ValidateProvenanceArtifact(dir, expected); err != nil {
		t.Fatalf("valid provenance rejected: %v", err)
	}
	for name, mutate := range map[string]func(*ProvenanceExpectation){
		"wrong build":   func(v *ProvenanceExpectation) { v.BuildID = "223e4567-e89b-42d3-a456-426614174000" },
		"wrong project": func(v *ProvenanceExpectation) { v.ProjectID = "p_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" },
		"wrong commit":  func(v *ProvenanceExpectation) { v.BuilderCommit = strings.Repeat("c", 40) },
		"wrong workflow": func(v *ProvenanceExpectation) {
			v.WorkflowRef = "owner/repo/.github/workflows/other.yml@refs/heads/main"
		},
	} {
		t.Run(name, func(t *testing.T) {
			other := expected
			mutate(&other)
			if _, err := ValidateProvenanceArtifact(dir, other); err == nil {
				t.Fatal("interchanged artifact accepted")
			}
		})
	}
}

func TestProvenanceRejectsWrongOperation(t *testing.T) {
	dir, expected := provenanceFixture(t)
	data, err := os.ReadFile(filepath.Join(dir, provenanceFile))
	if err != nil {
		t.Fatal(err)
	}
	var manifest ProvenanceManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Operation = "build"
	writeManifest(t, dir, &manifest)
	if _, err := ValidateProvenanceArtifact(dir, expected); err == nil {
		t.Fatal("provenance for the wrong operation was accepted")
	}
}

func TestProvenanceRejectsTamperingAndUnexpectedMembers(t *testing.T) {
	for name, attack := range map[string]func(*testing.T, string){
		"tampered ciphertext": func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, trustedIPAFile), []byte("replacement"), 0600); err != nil {
				t.Fatal(err)
			}
		},
		"tampered manifest": func(t *testing.T, dir string) {
			data, _ := os.ReadFile(filepath.Join(dir, provenanceFile))
			if err := os.WriteFile(filepath.Join(dir, provenanceFile), append(data, 'x'), 0600); err != nil {
				t.Fatal(err)
			}
		},
		"extra file": func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "injected"), []byte("x"), 0600); err != nil {
				t.Fatal(err)
			}
		},
		"nested member": func(t *testing.T, dir string) {
			if err := os.Mkdir(filepath.Join(dir, "nested"), 0700); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir, expected := provenanceFixture(t)
			attack(t, dir)
			if _, err := ValidateProvenanceArtifact(dir, expected); err == nil {
				t.Fatal("tampered provenance accepted")
			}
		})
	}
}

func TestProvenanceRejectsSymlink(t *testing.T) {
	dir, expected := provenanceFixture(t)
	if err := os.Remove(filepath.Join(dir, provenanceFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, trustedIPAFile), filepath.Join(dir, provenanceFile)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := ValidateProvenanceArtifact(dir, expected); err == nil {
		t.Fatal("symlink provenance accepted")
	}
}

func TestInvalidGitHubAttestationFailsClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX executable")
	}
	dir, _ := provenanceFixture(t)
	bin := t.TempDir()
	gh := filepath.Join(bin, "gh")
	if err := os.WriteFile(gh, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	err := VerifyGitHubAttestations(t.Context(), dir, "owner/repo", "owner/repo/.github/workflows/ios-build.yml", "refs/heads/main", strings.Repeat("a", 40), os.Stderr)
	if err == nil {
		t.Fatal("invalid provenance signature was accepted")
	}
}

func TestGitHubAttestationVerifiesBothExactSubjectsWithPolicy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX executable")
	}
	dir, _ := provenanceFixture(t)
	bin := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "args.log")
	gh := filepath.Join(bin, "gh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + logPath + "'\n"
	if err := os.WriteFile(gh, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	commit := strings.Repeat("a", 40)
	workflow := "owner/repo/.github/workflows/ios-build.yml"
	if err := VerifyGitHubAttestations(t.Context(), dir, "owner/repo", workflow, "refs/heads/main", commit, os.Stderr); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("gh invocation count = %d; args = %q", len(lines), data)
	}
	for index, name := range []string{trustedIPAFile, provenanceFile} {
		for _, required := range []string{
			"attestation verify " + filepath.Join(dir, name), "--repo owner/repo",
			"--signer-workflow " + workflow, "--signer-digest " + commit,
			"--source-digest " + commit, "--source-ref refs/heads/main",
			"--deny-self-hosted-runners", "--format json",
		} {
			if !strings.Contains(lines[index], required) {
				t.Errorf("verification invocation missing %q: %s", required, lines[index])
			}
		}
	}
}

func TestPlaintextIPATamperingFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "App.ipa")
	original := []byte("trusted plaintext IPA")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(original)
	manifest := &ProvenanceManifest{PlaintextIPASHA256: hex.EncodeToString(sum[:])}
	if err := verifyPlaintextIPADigest(path, manifest); err != nil {
		t.Fatalf("matching plaintext rejected: %v", err)
	}
	if err := os.WriteFile(path, []byte("substituted IPA"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyPlaintextIPADigest(path, manifest); err == nil {
		t.Fatal("tampered plaintext IPA accepted")
	}
}

func TestTrustedOutputRejectsPreplantedFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, trustedIPAFile), []byte("preplant"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := prepareExactOutputDir(dir); err == nil {
		t.Fatal("preplanted trusted output accepted")
	}
}
