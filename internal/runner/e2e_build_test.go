package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/MobAI-App/ios-builder/internal/ipacheck"
)

// requireXcodeToolchain skips unless the test can run a real unsigned iOS
// build: macOS with Xcode, plus XcodeGen to produce the generic fixture project.
// CI installs both on its macOS leg; on other platforms these tests are skipped
// and the build pipeline is covered only by the command-sequence tests.
func requireXcodeToolchain(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("real iOS builds need macOS")
	}
	for _, tool := range []string{"xcodebuild", "xcodegen"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
}

// writeFile creates a fixture file, making parent directories as needed.
func writeFile(t *testing.T, root, relative, contents string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

// writeGenericApp writes a minimal SwiftUI application described by an
// XcodeGen manifest under root/relative. Every name is a placeholder.
func writeGenericApp(t *testing.T, root, relative, bundleID string) {
	t.Helper()
	writeFile(t, root, relative+"/project.yml", `name: GenericApp
options:
  bundleIdPrefix: example.generic
targets:
  GenericApp:
    type: application
    platform: iOS
    deploymentTarget: "15.0"
    sources: [Sources]
    settings:
      base:
        PRODUCT_BUNDLE_IDENTIFIER: `+bundleID+`
        GENERATE_INFOPLIST_FILE: YES
        MARKETING_VERSION: 1.0
        CURRENT_PROJECT_VERSION: 1
        INFOPLIST_KEY_UILaunchScreen_Generation: YES
`)
	writeFile(t, root, relative+"/Sources/App.swift", `import SwiftUI

@main
struct GenericApp: App {
    var body: some Scene {
        WindowGroup { Text("Generic") }
    }
}
`)
}

// buildFixture runs the production unsigned pipeline against root and returns
// the inspected IPA. A passing result means xcodebuild produced an application
// that packaged into a structurally valid IPA with the expected identity.
func buildFixture(t *testing.T, root string, options *BuildOptions) *ipacheck.Info {
	t.Helper()
	writeFile(t, root, ".git/config", "[core]\n")
	output := filepath.Join(t.TempDir(), "private-output")
	options.SourceRoot = root
	options.LogPath = filepath.Join(output, "build.log")
	options.IPAPath = filepath.Join(output, "App.ipa")
	if options.Configuration == "" {
		options.Configuration = "Debug"
	}
	if err := BuildUnsigned(context.Background(), options); err != nil {
		log, _ := os.ReadFile(options.LogPath)
		t.Fatalf("unsigned build failed: %v\n%s", err, log)
	}
	info, err := ipacheck.Inspect(options.IPAPath)
	if err != nil {
		t.Fatalf("built IPA is invalid: %v", err)
	}
	return info
}

func TestE2EUnsignedNativeBuildProducesValidIPA(t *testing.T) {
	requireXcodeToolchain(t)
	root := t.TempDir()
	writeGenericApp(t, root, ".", "example.generic.app")
	generate := exec.Command("xcodegen", "generate")
	generate.Dir = root
	if out, err := generate.CombinedOutput(); err != nil {
		t.Fatalf("xcodegen generate: %v\n%s", err, out)
	}
	info := buildFixture(t, root, &BuildOptions{
		Framework: FrameworkNative, IOSPath: ".", BundleID: "example.generic.app",
	})
	if info.BundleID != "example.generic.app" || info.AppName != "GenericApp.app" {
		t.Fatalf("unexpected application: %+v", info)
	}
}
