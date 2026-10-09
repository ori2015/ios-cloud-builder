package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/MobAI-App/ios-builder/internal/config"
	"github.com/MobAI-App/ios-builder/internal/registry"
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

func registerFixture(t *testing.T, entry *registry.Project, cfg *config.Config) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the local registry backup requires POSIX mode bits")
	}
	value := registry.New()
	if err := value.Put(cfg.ProjectID, entry); err != nil {
		t.Fatal(err)
	}
	data, err := value.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "registry.json")
	if err := registry.SaveFile(path, data); err != nil {
		t.Fatal(err)
	}
	return path
}

func flutterRepo(t *testing.T) string {
	root := t.TempDir()
	writeProjectFile(t, root, "pubspec.yaml", "flutter:\n")
	writeProjectFile(t, root, "ios/Runner.xcodeproj/project.pbxproj", genericPbxproj)
	return root
}

func baseEntry() *registry.Project {
	return &registry.Project{
		Owner: "o", Repo: "r", IOSPath: "ios", Configuration: "Debug", FrameworkHint: "flutter",
		SnapshotNamespace: "11111111111111111111111111111111", BundleID: "example.generic.app",
	}
}

func baseConfig() *config.Config {
	return &config.Config{
		ProjectID: "p_0123456789abcdef0123456789abcdef", SnapshotNamespace: "11111111111111111111111111111111",
		GitHub: config.GitHubConfig{Owner: "o", Repo: "r"},
	}
}

func TestCheckRegistrationAcceptsCurrentEntry(t *testing.T) {
	root, cfg := flutterRepo(t), baseConfig()
	if err := checkRegistration(root, cfg, registerFixture(t, baseEntry(), cfg)); err != nil {
		t.Fatal(err)
	}
}

func TestCheckRegistrationFindsStaleEntry(t *testing.T) {
	root, cfg := flutterRepo(t), baseConfig()
	entry := baseEntry()
	entry.FrameworkHint = "native"
	entry.IOSPath = "old/ios"
	entry.BundleID = "example.generic.other"
	err := checkRegistration(root, cfg, registerFixture(t, entry, cfg))
	if err == nil {
		t.Fatal("stale registry accepted")
	}
	for _, want := range []string{`framework_hint is "native"`, `"flutter"`, `ios_path is "old/ios"`, "bundle_id", "builder central register"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message missing %q: %v", want, err)
		}
	}
}

func TestCheckRegistrationMissingEntryAndBackup(t *testing.T) {
	root, cfg := flutterRepo(t), baseConfig()
	other := baseConfig()
	other.ProjectID = "p_ffffffffffffffffffffffffffffffff"
	if err := checkRegistration(root, cfg, registerFixture(t, baseEntry(), other)); err == nil || !strings.Contains(err.Error(), "not in the local registry") {
		t.Fatalf("entry for another project accepted: %v", err)
	}
	if err := checkRegistration(root, cfg, filepath.Join(t.TempDir(), "absent.json")); err == nil || !strings.Contains(err.Error(), "builder central register") {
		t.Fatalf("missing backup not explained: %v", err)
	}
}

func TestCheckRegistrationFollowsBuilderJSONScheme(t *testing.T) {
	root, cfg := flutterRepo(t), baseConfig()
	cfg.IOS.Scheme = "Runner"
	if err := checkRegistration(root, cfg, registerFixture(t, baseEntry(), cfg)); err == nil || !strings.Contains(err.Error(), "scheme") {
		t.Fatalf("scheme drift not found: %v", err)
	}
}
