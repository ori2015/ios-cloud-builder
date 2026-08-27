package runner

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/MobAI-App/ios-builder/internal/registry"
)

func validInputs(t *testing.T) Inputs {
	t.Helper()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	buildID := "123e4567-e89b-42d3-a456-426614174000"
	return Inputs{
		BuildID:           buildID,
		ProjectID:         "p_0123456789abcdef0123456789abcdef",
		ArtifactRecipient: identity.Recipient().String(),
		Operation:         "build",
	}
}

func TestResolveOpaqueProjectMasksBeforeOutputs(t *testing.T) {
	in := validInputs(t)
	value := registry.New()
	project := &registry.Project{Owner: "private-owner", Repo: "private-repo", IOSPath: "ios", Scheme: "Private App", Configuration: "Debug", FrameworkHint: "auto", SnapshotNamespace: "11111111111111111111111111111111"}
	if err := value.Put(in.ProjectID, project); err != nil {
		t.Fatal(err)
	}
	data, _ := value.Marshal()
	outputPath := filepath.Join(t.TempDir(), "github-output")
	if err := os.WriteFile(outputPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	var commands bytes.Buffer
	if err := ResolveProject(&in, string(data), outputPath, &commands); err != nil {
		t.Fatal(err)
	}
	output, _ := os.ReadFile(outputPath)
	for _, private := range []string{"private-owner", "private-repo", "Private App", "refs/ios-builder/jobs/11111111111111111111111111111111/" + in.BuildID} {
		if !strings.Contains(commands.String(), "::add-mask::"+private) && !strings.Contains(commands.String(), private) {
			t.Errorf("mask command missing %q", private)
		}
		if !strings.Contains(string(output), private) {
			t.Errorf("trusted output missing %q", private)
		}
	}
	unknown := in
	unknown.ProjectID = "p_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := ResolveProject(&unknown, string(data), outputPath, &commands); err == nil {
		t.Fatal("unknown opaque project accepted")
	}
}

func TestResolveProjectRequiresPinnedBundleIDForSignedOperations(t *testing.T) {
	resolve := func(t *testing.T, operation, bundleID string) error {
		t.Helper()
		in := validInputs(t)
		in.Operation = operation
		value := registry.New()
		project := &registry.Project{
			Owner: "private-owner", Repo: "private-repo", BundleID: bundleID, IOSPath: "ios",
			Scheme: "Private App", Configuration: "Debug", FrameworkHint: "auto",
			SnapshotNamespace: "11111111111111111111111111111111",
		}
		if err := value.Put(in.ProjectID, project); err != nil {
			t.Fatal(err)
		}
		data, _ := value.Marshal()
		outputPath := filepath.Join(t.TempDir(), "github-output")
		if err := os.WriteFile(outputPath, nil, 0600); err != nil {
			t.Fatal(err)
		}
		var commands bytes.Buffer
		return ResolveProject(&in, string(data), outputPath, &commands)
	}

	// Unsigned builds never reach an Apple identity, so they stay unpinned.
	if err := resolve(t, OperationBuild, ""); err != nil {
		t.Fatalf("unsigned build rejected without a pinned bundle identifier: %v", err)
	}
	// Signing an unpinned project would take the identity from project-controlled
	// build output, so both signed operations must refuse.
	for _, operation := range []string{OperationTestFlight, OperationAdHoc} {
		t.Run(operation, func(t *testing.T) {
			if err := resolve(t, operation, ""); err == nil {
				t.Fatal("signed operation accepted a project with no pinned bundle identifier")
			}
			if err := resolve(t, operation, "com.example.app"); err != nil {
				t.Fatalf("pinned project rejected: %v", err)
			}
		})
	}
}

func TestVerifyBuiltBundleIDRejectsMismatch(t *testing.T) {
	appPath := filepath.Join(t.TempDir(), "App.app")
	if err := os.MkdirAll(appPath, 0700); err != nil {
		t.Fatal(err)
	}
	info := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>com.example.app</string>
<key>CFBundleShortVersionString</key><string>1.0</string>
</dict></plist>`
	if err := os.WriteFile(filepath.Join(appPath, "Info.plist"), []byte(info), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyBuiltBundleID(appPath, "com.example.app"); err != nil {
		t.Fatalf("matching bundle identifier rejected: %v", err)
	}
	// The whole point: a project must not be able to build itself as another
	// application in the same Apple team and inherit its entitlements.
	if err := verifyBuiltBundleID(appPath, "com.example.other"); err == nil {
		t.Fatal("mismatched bundle identifier accepted")
	}
	if err := verifyBuiltBundleID(appPath, ""); err != nil {
		t.Fatalf("unpinned project rejected: %v", err)
	}
}

func TestInputsValidate(t *testing.T) {
	valid := validInputs(t)
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid inputs rejected: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*Inputs)
	}{
		{"short build id", func(in *Inputs) { in.BuildID = "12345678" }},
		{"uppercase UUID", func(in *Inputs) { in.BuildID = strings.ToUpper(in.BuildID) }},
		{"non-v4 UUID", func(in *Inputs) { in.BuildID = "123e4567-e89b-72d3-a456-426614174000" }},
		{"project id", func(in *Inputs) { in.ProjectID = "private-owner-repo" }},
		{"recipient", func(in *Inputs) { in.ArtifactRecipient = "age1invalid" }},
		{"operation", func(in *Inputs) { in.Operation = "shell" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := validInputs(t)
			test.mutate(&in)
			if err := in.Validate(); err == nil {
				t.Fatal("invalid inputs accepted")
			}
		})
	}
}

func TestChildEnvironmentScrubsActionsAndCredentials(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "secret")
	t.Setenv("GITHUB_WORKSPACE", "/private")
	t.Setenv("ACTIONS_RUNTIME_TOKEN", "secret")
	t.Setenv("RUNNER_TEMP", "/runner")
	t.Setenv("APP_PRIVATE_KEY", "secret")
	t.Setenv("AGE_RECIPIENT", "public-but-not-needed")
	t.Setenv("APPLE_SIGNING_RECIPIENT", "public-but-not-needed")
	t.Setenv("ARTIFACT_RECIPIENT", "public-but-not-needed")
	t.Setenv("PACKAGING_RECIPIENT", "public-but-not-needed")
	t.Setenv("PACKAGING_AGE_IDENTITY", "secret")
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("HOME", "/sensitive-runner-home")
	t.Setenv("JAVA_HOME", "/java")
	env := strings.Join(ChildEnvironment("/source", "/isolated-home"), "\n")
	for _, forbidden := range []string{"GITHUB_", "ACTIONS_", "RUNNER_", "APP_PRIVATE_KEY", "AGE_RECIPIENT", "RECIPIENT", "PACKAGING_", "secret", "/sensitive-runner-home"} {
		if strings.Contains(env, forbidden) {
			t.Fatalf("child environment leaked %q: %s", forbidden, env)
		}
	}
	for _, required := range []string{"PATH=/usr/bin", "HOME=/isolated-home", "JAVA_HOME=/java", "CODE_SIGNING_ALLOWED=NO"} {
		if !strings.Contains(env, required) {
			t.Fatalf("child environment missing %q: %s", required, env)
		}
	}
}

func TestDetectFramework(t *testing.T) {
	tests := []struct {
		name, path, content, want string
	}{
		{"flutter", "pubspec.yaml", "name: app", FrameworkFlutter},
		{"expo", "package.json", `{"dependencies":{"expo":"1"}}`, FrameworkExpo},
		{"react native", "package.json", `{"dependencies":{"react-native":"1"}}`, FrameworkReactNative},
		{"ionic", "package.json", `{"dependencies":{"@ionic/core":"1"}}`, FrameworkIonic},
		{"cordova", "config.xml", "<widget/>", FrameworkCordova},
		{"kmp", "shared/build.gradle.kts", `plugins { kotlin("multiplatform") }`, FrameworkKMP},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, test.path)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(test.content), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := DetectFramework(root, FrameworkAuto)
			if err != nil || got != test.want {
				t.Fatalf("DetectFramework() = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestCapacitorAndXcodeGenDetection(t *testing.T) {
	root := t.TempDir()
	if isCapacitorProject(root) {
		t.Fatal("empty project detected as Capacitor")
	}
	if err := os.WriteFile(filepath.Join(root, "capacitor.config.ts"), []byte("export default {}"), 0600); err != nil {
		t.Fatal(err)
	}
	if !isCapacitorProject(root) {
		t.Fatal("Capacitor config was not detected")
	}
	if hasXcodeGenManifest(root) {
		t.Fatal("Capacitor config detected as XcodeGen")
	}
	if err := os.WriteFile(filepath.Join(root, "project.yml"), []byte("name: App\ntargets:\n  App: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if !hasXcodeGenManifest(root) {
		t.Fatal("XcodeGen targets manifest was not detected")
	}
}

func TestMakeGradleWrapperExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose POSIX executable mode bits")
	}
	root := t.TempDir()
	wrapper := filepath.Join(root, "gradlew")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := makeGradleWrapperExecutable(root); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(wrapper)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0111 == 0 {
		t.Fatalf("gradlew mode = %o; expected executable", info.Mode().Perm())
	}
}

func TestEncryptArtifactsRoundTripAndDeletesPlaintext(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	logPath := filepath.Join(root, "plain", "build.log")
	ipaPath := filepath.Join(root, "plain", "App.ipa")
	if err := os.MkdirAll(filepath.Dir(logPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("private compiler diagnostics"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ipaPath, []byte("private ipa"), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "encrypted")
	if err := EncryptArtifacts(identity.Recipient().String(), logPath, ipaPath, output); err != nil {
		t.Fatal(err)
	}
	for _, plain := range []string{logPath, ipaPath} {
		if _, err := os.Stat(plain); !os.IsNotExist(err) {
			t.Fatalf("plaintext was not deleted: %s", plain)
		}
	}
	entries, err := os.ReadDir(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name() != "App.ipa.age" || entries[1].Name() != "build.log.age" {
		t.Fatalf("unexpected ciphertext members: %v", entries)
	}
	got := decryptFile(t, filepath.Join(output, "App.ipa.age"), identity)
	if got != "private ipa" {
		t.Fatalf("decrypted IPA = %q", got)
	}
}

func TestEncryptArtifactsFailureStillDeletesPlaintext(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "build.log")
	if err := os.WriteFile(logPath, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := EncryptArtifacts("invalid", logPath, "", filepath.Join(root, "out")); err == nil {
		t.Fatal("invalid recipient accepted")
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatal("plaintext retained after encryption failure")
	}
}

func TestEncryptArtifactsRejectsUnexpectedOutputMembers(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	logPath := filepath.Join(root, "build.log")
	output := filepath.Join(root, "encrypted")
	if err := os.Mkdir(output, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(output, "private-source.txt"), []byte("must not upload"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(output, "App.ipa.age"), []byte("untrusted preplant"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := EncryptArtifacts(identity.Recipient().String(), logPath, "", output); err == nil {
		t.Fatal("unexpected output member was accepted")
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatal("plaintext log retained after output validation failure")
	}
	if _, err := os.Stat(filepath.Join(output, "App.ipa.age")); !os.IsNotExist(err) {
		t.Fatal("untrusted allowlisted preplant was retained")
	}
}

func TestBuildOptionsRequireFixedPrivateOutput(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	options := BuildOptions{
		SourceRoot: source, IOSPath: ".", Configuration: "Release", Framework: FrameworkNative,
		LogPath: filepath.Join(root, "private-output", "build.log"),
		IPAPath: filepath.Join(root, "private-output", "App.ipa"),
	}
	if err := options.validate(); err != nil {
		t.Fatalf("fixed output paths rejected: %v", err)
	}
	options.IPAPath = filepath.Join(root, "private-output", "source.zip")
	if err := options.validate(); err == nil {
		t.Fatal("arbitrary private output name accepted")
	}
}

func decryptFile(t *testing.T, path string, identity age.Identity) string {
	t.Helper()
	ciphertext, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ciphertext.Close()
	reader, err := age.Decrypt(ciphertext, identity)
	if err != nil {
		t.Fatal(err)
	}
	var plain bytes.Buffer
	if _, err := io.Copy(&plain, reader); err != nil {
		t.Fatal(err)
	}
	return plain.String()
}

func TestVerifyCheckoutNoCredentials(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, ".git", "config")
	if err := os.WriteFile(config, []byte("[remote \"origin\"]\nurl = https://github.com/o/r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyCheckoutNoCredentials(root); err != nil {
		t.Fatalf("clean checkout rejected: %v", err)
	}
	if err := os.WriteFile(config, []byte("[http \"https://github.com/\"]\nextraheader = AUTHORIZATION: basic secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyCheckoutNoCredentials(root); err == nil {
		t.Fatal("persisted checkout credential accepted")
	}
}
