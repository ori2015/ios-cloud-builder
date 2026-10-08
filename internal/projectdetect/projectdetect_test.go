package projectdetect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, root, rel, contents string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, root, rel string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(rel)), 0o755); err != nil {
		t.Fatal(err)
	}
}

const (
	flutterPubspec = "name: generic\nflutter:\n  uses-material-design: true\n"
	expoPackage    = `{"dependencies":{"expo":"~50.0.0","react-native":"0.73.0"}}`
	rnPackage      = `{"dependencies":{"react-native":"0.73.0"}}`
	capPackage     = `{"dependencies":{"@capacitor/core":"5.0.0","@capacitor/ios":"5.0.0"}}`
	xcodegenYML    = "name: App\ntargets:\n  App:\n    type: application\n"
	cordovaConfig  = `<widget id="com.example.generic" xmlns="http://www.w3.org/ns/widgets" xmlns:cdv="http://cordova.apache.org/ns/1.0"></widget>`
)

func resolve(t *testing.T, root, appPath string) *Layout {
	t.Helper()
	layout, err := Resolve(root, appPath)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return layout
}

func TestResolveLayouts(t *testing.T) {
	cases := []struct {
		name      string
		setup     func(t *testing.T, root string)
		appPath   string
		wantApp   string
		wantIOS   string
		wantKind  Kind
		generated bool
	}{
		{"flutter at root", func(t *testing.T, r string) {
			write(t, r, "pubspec.yaml", flutterPubspec)
			mkdir(t, r, "ios/Runner.xcodeproj")
		}, "", ".", "ios", KindFlutter, false},
		{"flutter in subfolder", func(t *testing.T, r string) {
			write(t, r, "mobile/pubspec.yaml", flutterPubspec)
			mkdir(t, r, "mobile/ios/Runner.xcodeproj")
			write(t, r, "README.md", "x")
		}, "", "mobile", "mobile/ios", KindFlutter, false},
		{"managed expo", func(t *testing.T, r string) {
			write(t, r, "apps/client/package.json", expoPackage)
		}, "", "apps/client", "apps/client/ios", KindNode, true},
		{"react native with ios", func(t *testing.T, r string) {
			write(t, r, "package.json", rnPackage)
			mkdir(t, r, "ios/Generic.xcodeproj")
		}, "", ".", "ios", KindNode, false},
		{"capacitor ios/App", func(t *testing.T, r string) {
			write(t, r, "package.json", capPackage)
			write(t, r, "capacitor.config.ts", "export default {}")
			mkdir(t, r, "ios/App/App.xcodeproj")
		}, "", ".", "ios/App", KindNode, false},
		{"cordova prepared", func(t *testing.T, r string) {
			write(t, r, "config.xml", cordovaConfig)
			mkdir(t, r, "platforms/ios/Generic.xcodeproj")
		}, "", ".", "platforms/ios", KindNode, false},
		{"cordova unprepared", func(t *testing.T, r string) {
			write(t, r, "config.xml", cordovaConfig)
		}, "", ".", "platforms/ios", KindNode, true},
		{"native xcodeproj at root", func(t *testing.T, r string) {
			mkdir(t, r, "Generic.xcodeproj")
		}, "", ".", ".", KindXcode, false},
		{"native in subfolder", func(t *testing.T, r string) {
			mkdir(t, r, "App/Generic.xcodeproj")
		}, "", "App", "App", KindXcode, false},
		{"xcodegen at root", func(t *testing.T, r string) {
			write(t, r, "project.yml", xcodegenYML)
		}, "", ".", ".", KindXcodeGen, false},
		{"kmp with iosApp", func(t *testing.T, r string) {
			write(t, r, "settings.gradle.kts", "rootProject.name = \"generic\"")
			mkdir(t, r, "iosApp/iosApp.xcodeproj")
		}, "", ".", "iosApp", KindKMP, false},
		{"monorepo with explicit app path", func(t *testing.T, r string) {
			write(t, r, "apps/a/pubspec.yaml", flutterPubspec)
			mkdir(t, r, "apps/a/ios/Runner.xcodeproj")
			write(t, r, "apps/b/pubspec.yaml", flutterPubspec)
			mkdir(t, r, "apps/b/ios/Runner.xcodeproj")
		}, "apps/b", "apps/b", "apps/b/ios", KindFlutter, false},
		{"dependencies and pods are ignored", func(t *testing.T, r string) {
			write(t, r, "package.json", rnPackage)
			mkdir(t, r, "ios/Generic.xcodeproj")
			mkdir(t, r, "ios/Pods/Pods.xcodeproj")
			write(t, r, "node_modules/some-lib/package.json", expoPackage)
			mkdir(t, r, "node_modules/some-lib/ios/Lib.xcodeproj")
		}, "", ".", "ios", KindNode, false},
		{"tooling package.json does not hide a native project", func(t *testing.T, r string) {
			write(t, r, "package.json", `{"devDependencies":{"prettier":"3.0.0"}}`)
			mkdir(t, r, "Generic.xcodeproj")
		}, "", ".", ".", KindXcode, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tc.setup(t, root)
			got := resolve(t, root, tc.appPath)
			if got.AppPath != tc.wantApp || got.IOSPath != tc.wantIOS || got.Kind != tc.wantKind || got.Generated != tc.generated {
				t.Fatalf("got %+v, want app=%q ios=%q kind=%s generated=%v", *got, tc.wantApp, tc.wantIOS, tc.wantKind, tc.generated)
			}
		})
	}
}

func TestResolveAmbiguousListsCandidates(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a/pubspec.yaml", flutterPubspec)
	mkdir(t, root, "a/ios/Runner.xcodeproj")
	write(t, root, "b/package.json", expoPackage)
	_, err := Resolve(root, "")
	if err == nil || !strings.Contains(err.Error(), "a (flutter)") || !strings.Contains(err.Error(), "b (node)") ||
		!strings.Contains(err.Error(), "--app-path") {
		t.Fatalf("error does not name the candidates and the flag: %v", err)
	}
}

func TestResolveNothingFoundNamesWhatWasSought(t *testing.T) {
	root := t.TempDir()
	write(t, root, "README.md", "x")
	_, err := Resolve(root, "")
	if err == nil || !strings.Contains(err.Error(), "pubspec.yaml") || !strings.Contains(err.Error(), "--app-path") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

func TestResolveRejectsBadAppPath(t *testing.T) {
	root := t.TempDir()
	write(t, root, "x/readme", "x")
	for _, bad := range []string{"../escape", "/abs", "missing", `back\slash`, "x"} {
		if _, err := Resolve(root, bad); err == nil {
			t.Errorf("app path %q accepted", bad)
		}
	}
}

func TestScanDepthIsBounded(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a/b/c/d/e/package.json", expoPackage)
	if _, err := Resolve(root, ""); err == nil {
		t.Fatal("app deeper than MaxDepth found without --app-path")
	}
	if layout := resolve(t, root, "a/b/c/d/e"); layout.AppPath != "a/b/c/d/e" {
		t.Fatalf("explicit deep path not honoured: %+v", layout)
	}
}

func TestUsableWorkspace(t *testing.T) {
	root := t.TempDir()
	const header = `<?xml version="1.0" encoding="UTF-8"?><Workspace version = "1.0">`
	ref := func(loc string) string { return `<FileRef location = "` + loc + `"></FileRef>` }

	mkdir(t, root, "Empty.xcworkspace")
	write(t, root, "Stub.xcworkspace/contents.xcworkspacedata", header+"</Workspace>")
	write(t, root, "Missing.xcworkspace/contents.xcworkspacedata", header+ref("group:Gone.xcodeproj")+"</Workspace>")
	mkdir(t, root, "Real.xcodeproj")
	write(t, root, "Real.xcworkspace/contents.xcworkspacedata", header+ref("group:Real.xcodeproj")+"</Workspace>")
	write(t, root, "Pods.xcworkspace/contents.xcworkspacedata", header+ref("group:Real.xcodeproj")+ref("group:Pods/Pods.xcodeproj")+"</Workspace>")

	want := map[string]bool{"Empty": false, "Stub": false, "Missing": false, "Real": true, "Pods": true}
	for name, usable := range want {
		if got := UsableWorkspace(filepath.Join(root, name+".xcworkspace")); got != usable {
			t.Errorf("%s.xcworkspace usable = %v, want %v", name, got, usable)
		}
	}
}

func TestStubWorkspaceDoesNotCountAsContainer(t *testing.T) {
	root := t.TempDir()
	mkdir(t, root, "App.xcworkspace")
	write(t, root, "project.yml", xcodegenYML)
	got := resolve(t, root, "")
	if got.Kind != KindXcodeGen {
		t.Fatalf("stub workspace hid the XcodeGen manifest: %+v", *got)
	}
}

func TestPickScheme(t *testing.T) {
	cases := []struct {
		name      string
		schemes   []string
		container string
		want      string
		wantErr   string
	}{
		{"exact container match wins", []string{"AppTests", "Generic", "GenericWidget"}, "Generic", "Generic", ""},
		{"single non-test scheme", []string{"GenericTests", "GenericUITests", "Main"}, "Other", "Main", ""},
		{"pods and extensions skipped", []string{"Pods-Generic", "Alamofire-Extension", "Generic"}, "x", "Generic", ""},
		{"ambiguous lists names", []string{"One", "Two"}, "x", "", "One, Two"},
		{"suffix-like app name still matches its container", []string{"StopWatch", "StopWatchTests"}, "StopWatch", "StopWatch", ""},
		{"only tests", []string{"GenericTests"}, "x", "", "no application scheme"},
		{"a scheme literally named Tests is not stripped to nothing", []string{"Tests"}, "x", "Tests", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PickScheme(tc.schemes, tc.container)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
