package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MobAI-App/ios-builder/internal/config"
)

func writeProjectFile(t *testing.T, root, rel, contents string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

const genericPbxproj = "{\n\tobjects = {\n\t\tA1 = {\n\t\t\tisa = XCBuildConfiguration;\n\t\t\tbuildSettings = {\n" +
	"\t\t\t\tPRODUCT_BUNDLE_IDENTIFIER = example.generic.app;\n\t\t\t};\n\t\t\tname = Debug;\n\t\t};\n\t};\n}\n"

func TestReadProjectFactsFlutterMonorepo(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, "mobile/pubspec.yaml", "name: generic\nflutter:\n  uses-material-design: true\n")
	writeProjectFile(t, root, "mobile/ios/Runner.xcodeproj/project.pbxproj", genericPbxproj)
	facts, err := readProjectFacts(root, &config.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if facts.AppPath != "mobile" || facts.IOSPath != "mobile/ios" || facts.Framework != "flutter" || facts.BundleID != "example.generic.app" {
		t.Fatalf("unexpected facts: %+v", *facts)
	}
}

func TestReadProjectFactsManagedExpoNeedsNoConfigFlags(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, "package.json", `{"dependencies":{"expo":"~50.0.0"}}`)
	writeProjectFile(t, root, "app.json", `{"expo":{"ios":{"bundleIdentifier":"example.generic.expo"}}}`)
	facts, err := readProjectFacts(root, &config.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if facts.Framework != "expo" || facts.IOSPath != "ios" || facts.BundleID != "example.generic.expo" || facts.AppPath != "" {
		t.Fatalf("unexpected facts: %+v", *facts)
	}
}

func TestReadProjectFactsBundleFlagOverridesAndUnpinnedIsExplained(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, "package.json", `{"dependencies":{"expo":"~50.0.0"}}`)
	facts, err := readProjectFacts(root, &config.Config{}, "example.generic.flag")
	if err != nil || facts.BundleID != "example.generic.flag" {
		t.Fatalf("flag ignored: %+v %v", facts, err)
	}
	facts, err = readProjectFacts(root, &config.Config{}, "")
	if err != nil || facts.BundleID != "" || !strings.Contains(describeNotes(facts.Notes), "not pinned") {
		t.Fatalf("unpinned project not explained: %+v %v", facts, err)
	}
}

func TestReadProjectFactsKeepsBuilderJSONButReportsDisagreement(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, "ios/Runner.xcodeproj/project.pbxproj", genericPbxproj)
	writeProjectFile(t, root, "pubspec.yaml", "flutter:\n")
	cfg := &config.Config{IOS: config.IOSConfig{Path: "old/ios"}}
	facts, err := readProjectFacts(root, cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	if facts.IOSPath != "old/ios" || !strings.Contains(describeNotes(facts.Notes), `"ios"`) {
		t.Fatalf("stale ios.path not reported: %+v", *facts)
	}
}

func TestReadProjectFactsErrorsAreActionable(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, "README.md", "x")
	if _, err := readProjectFacts(root, &config.Config{}, ""); err == nil || !strings.Contains(err.Error(), "--app-path") {
		t.Fatalf("nothing found: %v", err)
	}
	root = t.TempDir()
	writeProjectFile(t, root, "a/pubspec.yaml", "flutter:\n")
	writeProjectFile(t, root, "a/ios/Runner.xcodeproj/project.pbxproj", genericPbxproj)
	writeProjectFile(t, root, "b/pubspec.yaml", "flutter:\n")
	writeProjectFile(t, root, "b/ios/Runner.xcodeproj/project.pbxproj", genericPbxproj)
	if _, err := readProjectFacts(root, &config.Config{}, ""); err == nil || !strings.Contains(err.Error(), "a (flutter), b (flutter)") {
		t.Fatalf("ambiguous repo: %v", err)
	}
	cfg := &config.Config{IOS: config.IOSConfig{AppPath: "b"}}
	if facts, err := readProjectFacts(root, cfg, ""); err != nil || facts.AppPath != "b" {
		t.Fatalf("explicit app path: %+v %v", facts, err)
	}
}
