package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MobAI-App/ios-builder/internal/registry"
)

func TestResolveAppRoot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "apps/mobile/package.json", "{}")
	writeFile(t, root, "apps/mobile/pubspec.yaml", "flutter:\n")
	writeFile(t, root, "package.json", "{}")
	if err := os.MkdirAll(filepath.Join(root, "apps", "mobile", "ios"), 0o700); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		options BuildOptions
		want    string
		wantErr bool
	}{
		{"explicit app path wins", BuildOptions{Framework: FrameworkExpo, AppPath: "apps/mobile", IOSPath: "ios"}, filepath.Join(real, "apps", "mobile"), false},
		{"explicit root", BuildOptions{Framework: FrameworkFlutter, AppPath: ".", IOSPath: "ios"}, real, false},
		{"missing explicit path fails with guidance", BuildOptions{Framework: FrameworkExpo, AppPath: "apps/gone", IOSPath: "ios"}, "", true},
		{"legacy node rule", BuildOptions{Framework: FrameworkExpo, IOSPath: "apps/mobile/ios"}, filepath.Join(real, "apps", "mobile"), false},
		{"legacy flutter rule", BuildOptions{Framework: FrameworkFlutter, IOSPath: "apps/mobile/ios"}, filepath.Join(real, "apps", "mobile"), false},
		{"legacy native stays at root", BuildOptions{Framework: FrameworkNative, IOSPath: "apps/mobile/ios"}, real, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveAppRoot(real, &tc.options)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "builder central register") {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestResolveAppRootRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "app")); err != nil {
		t.Skip("symlinks unavailable")
	}
	real, _ := filepath.EvalSymlinks(root)
	if _, err := resolveAppRoot(real, &BuildOptions{Framework: FrameworkExpo, AppPath: "app"}); err == nil {
		t.Fatal("app path escaping the checkout through a symlink was accepted")
	}
}

func TestBuildOptionsValidateAppPath(t *testing.T) {
	base := func() BuildOptions {
		dir := t.TempDir()
		return BuildOptions{
			Framework: FrameworkNative, Configuration: "Debug", IOSPath: ".", SourceRoot: filepath.Join(dir, "src"),
			LogPath: filepath.Join(dir, "private-output", "build.log"), IPAPath: filepath.Join(dir, "private-output", "App.ipa"),
		}
	}
	good := base()
	good.AppPath = "apps/mobile"
	if err := good.validate(); err != nil {
		t.Fatalf("valid app path rejected: %v", err)
	}
	for _, bad := range []string{"../x", "/abs", "a/../b", `a\b`} {
		options := base()
		options.AppPath = bad
		if err := options.validate(); err == nil {
			t.Errorf("app path %q accepted", bad)
		}
	}
}

func TestDetectFrameworkAtUsesAppFolder(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "package.json", `{"devDependencies":{"prettier":"3"}}`)
	writeFile(t, root, "apps/mobile/package.json", `{"dependencies":{"expo":"50","react-native":"0.73"}}`)
	writeFile(t, root, "apps/other/pubspec.yaml", "flutter:\n")
	real, _ := filepath.EvalSymlinks(root)
	for appPath, want := range map[string]string{"apps/mobile": FrameworkExpo, "apps/other": FrameworkFlutter, "": FrameworkNative, ".": FrameworkNative} {
		got, err := DetectFrameworkAt(real, appPath, FrameworkAuto)
		if err != nil || got != want {
			t.Errorf("DetectFrameworkAt(%q) = %q, %v; want %q", appPath, got, err, want)
		}
	}
	if _, err := DetectFrameworkAt(real, "../escape", FrameworkAuto); err == nil {
		t.Error("escaping app path accepted")
	}
	if _, err := DetectFrameworkAt(real, "apps/gone", FrameworkAuto); err == nil {
		t.Error("missing app path accepted")
	}
}

func TestFindXcodeContainerSkipsDanglingWorkspaceStub(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "App.xcworkspace/contents.xcworkspacedata",
		`<Workspace version = "1.0"><FileRef location = "group:App.xcodeproj"></FileRef></Workspace>`)
	if _, _, err := findXcodeContainer(root); err == nil {
		t.Fatal("workspace pointing at a project that is not committed was treated as a container")
	}
	// Once the project exists (XcodeGen generated it) the workspace is usable.
	if err := os.MkdirAll(filepath.Join(root, "App.xcodeproj"), 0o700); err != nil {
		t.Fatal(err)
	}
	workspace, project, err := findXcodeContainer(root)
	if err != nil || workspace != "App.xcworkspace" || project != "" {
		t.Fatalf("got %q %q %v", workspace, project, err)
	}
	// An empty stub never counts, but a real project beside it does.
	empty := t.TempDir()
	if err := os.MkdirAll(filepath.Join(empty, "Stub.xcworkspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(empty, "Real.xcodeproj"), 0o700); err != nil {
		t.Fatal(err)
	}
	workspace, project, err = findXcodeContainer(empty)
	if err != nil || workspace != "" || project != "Real.xcodeproj" {
		t.Fatalf("got %q %q %v", workspace, project, err)
	}
}

func TestResolveProjectEmitsAndMasksAppPath(t *testing.T) {
	in := validInputs(t)
	value := registry.New()
	project := &registry.Project{
		Owner: "owner-x", Repo: "repo-x", AppPath: "apps/mobile-app", IOSPath: "apps/mobile-app/ios",
		Configuration: "Debug", FrameworkHint: "auto", SnapshotNamespace: "11111111111111111111111111111111",
	}
	if err := value.Put(in.ProjectID, project); err != nil {
		t.Fatal(err)
	}
	data, _ := value.Marshal()
	outputPath := filepath.Join(t.TempDir(), "github-output")
	if err := os.WriteFile(outputPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var commands strings.Builder
	if err := ResolveProject(&in, string(data), outputPath, &commands); err != nil {
		t.Fatal(err)
	}
	output, _ := os.ReadFile(outputPath)
	if !strings.Contains(string(output), "app_path=apps/mobile-app\n") {
		t.Errorf("app_path missing from trusted outputs: %s", output)
	}
	if !strings.Contains(commands.String(), "::add-mask::apps/mobile-app\n") {
		t.Error("app path was not masked")
	}
}

func TestRegistryAppPathValidation(t *testing.T) {
	base := registry.Project{Owner: "o", Repo: "r", Configuration: "Debug", FrameworkHint: "auto", SnapshotNamespace: "11111111111111111111111111111111"}
	for _, ok := range []string{"", ".", "apps/a"} {
		p := base
		p.AppPath = ok
		if err := p.Validate(); err != nil {
			t.Errorf("app path %q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"..", "../x", "/abs", "a/./b", `a\b`, "a\nb"} {
		p := base
		p.AppPath = bad
		if err := p.Validate(); err == nil {
			t.Errorf("app path %q accepted", bad)
		}
	}
}

func TestMissingContainerErrorSaysWhatWasFoundAndHowToFix(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "README.md", "x")
	writeFile(t, root, "Gemfile", "x")
	_, _, err := findXcodeContainer(root)
	if err == nil {
		t.Fatal("no container accepted")
	}
	for _, want := range []string{"README.md", "Gemfile", "ios.path", "project.yml", "builder central register"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message missing %q: %v", want, err)
		}
	}
	if _, _, err := findXcodeContainer(filepath.Join(root, "absent")); err == nil || !strings.Contains(err.Error(), "empty or unreadable") {
		t.Errorf("absent folder not described: %v", err)
	}
}
