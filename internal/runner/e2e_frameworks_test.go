package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MobAI-App/ios-builder/internal/projectdetect"
)

// These tests scaffold a real project with the framework's own generator and
// run the production unsigned pipeline on it. They need macOS, Xcode, the
// framework toolchain and the network, so they run only where
// BUILDER_E2E_FRAMEWORKS names them (comma-separated, or "all").
func requireFrameworkE2E(t *testing.T, name string) {
	t.Helper()
	requested := strings.Split(os.Getenv("BUILDER_E2E_FRAMEWORKS"), ",")
	if !slices.Contains(requested, name) && !slices.Contains(requested, "all") {
		t.Skipf("set BUILDER_E2E_FRAMEWORKS=%s to run this real build", name)
	}
	if _, err := exec.LookPath("xcodebuild"); err != nil {
		t.Skip("xcodebuild not installed")
	}
}

// scaffold runs a generator in dir with the real environment.
func scaffold(t *testing.T, dir string, program string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", program, strings.Join(args, " "), err, out)
	}
}

// buildDetected resolves the layout, framework and bundle identifier the way
// `central register` does, then builds with exactly those values.
func buildDetected(t *testing.T, root, wantFramework string) {
	t.Helper()
	buildDetectedWith(t, root, wantFramework, nil)
}

// buildDetectedWith is buildDetected with a hook to set extra build options.
func buildDetectedWith(t *testing.T, root, wantFramework string, adjust func(*BuildOptions)) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		writeFile(t, root, ".git/config", "[core]\n")
	}
	layout, err := projectdetect.Resolve(root, "")
	if err != nil {
		t.Fatalf("detect layout: %v", err)
	}
	framework, err := projectdetect.DetectFramework(filepath.Join(root, filepath.FromSlash(layout.AppPath)))
	if err != nil || framework != wantFramework {
		t.Fatalf("detected framework %q, %v; want %q", framework, err, wantFramework)
	}
	bundleID, reason := projectdetect.BundleID(root, layout, "Debug")
	t.Logf("layout=%+v framework=%s bundle=%q (%s)", *layout, framework, bundleID, reason)
	iosPath := layout.IOSPath
	if iosPath == "" {
		iosPath = "."
	}
	appPath := layout.AppPath
	if appPath == "." {
		appPath = ""
	}
	options := &BuildOptions{Framework: framework, AppPath: appPath, IOSPath: iosPath, BundleID: bundleID}
	if adjust != nil {
		adjust(options)
	}
	info := buildFixture(t, root, options)
	t.Logf("built %s (%s)", info.AppName, info.BundleID)
}

func TestE2EFrameworkTauri(t *testing.T) {
	requireFrameworkE2E(t, "tauri")
	root := t.TempDir()
	scaffold(t, root, "npm", "create", "tauri-app@latest", "app", "--", "--template", "vanilla", "--manager", "npm",
		"--identifier", "example.generic.tauri", "--yes")
	buildDetected(t, root, FrameworkTauri)
}

func TestE2EFrameworkNativeScript(t *testing.T) {
	requireFrameworkE2E(t, "nativescript")
	root := t.TempDir()
	scaffold(t, root, "npx", "--yes", "nativescript@latest", "create", "generic",
		"--template", "@nativescript/template-blank-ts", "--appid", "example.generic.ns")
	buildDetected(t, filepath.Join(root, "generic"), FrameworkNativeScript)
}

func TestE2EFrameworkSparkling(t *testing.T) {
	requireFrameworkE2E(t, "sparkling")
	root := t.TempDir()
	scaffold(t, root, "npm", "create", "sparkling-app@latest", "my-app", "--", "--yes")
	buildDetected(t, filepath.Join(root, "my-app"), FrameworkSparkling)
}

// A generated .NET MAUI app on .NET 8, whose iOS workload matches the Xcode the
// runner images ship by default. (The newest .NET releases pull a workload that
// needs a newer Xcode than the image has; the pipeline explains that case.)
func TestE2EFrameworkMAUI(t *testing.T) {
	requireFrameworkE2E(t, "maui")
	root := t.TempDir()
	writeFile(t, root, "global.json", `{"sdk":{"version":"8.0.424","rollForward":"latestFeature"}}`)
	scaffold(t, root, "dotnet", "new", "install", "Microsoft.Maui.Templates.net8")
	scaffold(t, root, "dotnet", "new", "maui", "-n", "Generic", "-o", "app", "--framework", "net8.0")
	buildDetected(t, root, FrameworkMAUI)
}

// A hand-written Godot 4 project with an iOS export preset, the way the editor
// writes it. GODOT_DIR is the folder scripts/install-godot.sh filled.
func TestE2EFrameworkGodot(t *testing.T) {
	requireFrameworkE2E(t, "godot")
	godotDir := os.Getenv("GODOT_DIR")
	if godotDir == "" {
		t.Skip("GODOT_DIR is not set")
	}
	root := t.TempDir()
	writeFile(t, root, "game/project.godot", "config_version=5\n\n[application]\n\nconfig/name=\"Generic\"\nrun/main_scene=\"res://main.tscn\"\nconfig/features=PackedStringArray(\"4.4\", \"Mobile\")\nconfig/icon=\"res://icon.svg\"\n")
	writeFile(t, root, "game/main.tscn", "[gd_scene format=3]\n\n[node name=\"Main\" type=\"Node2D\"]\n")
	writeFile(t, root, "game/icon.svg", `<svg xmlns="http://www.w3.org/2000/svg" width="128" height="128"><rect width="128" height="128" fill="red"/></svg>`)
	writeFile(t, root, "game/export_presets.cfg", `[preset.0]

name="iOS"
platform="iOS"
runnable=true
advanced_options=false
dedicated_server=false
custom_features=""
export_filter="all_resources"
include_filter=""
exclude_filter=""
export_path=""
encryption_include_filters=""
encryption_exclude_filters=""
encrypt_pck=false
encrypt_directory=false

[preset.0.options]

application/bundle_identifier="example.generic.godot"
application/short_version="1.0"
application/version="1.0"
application/min_ios_version="14.0"
`)
	layoutRoot := filepath.Join(root, "game")
	buildDetectedWith(t, layoutRoot, FrameworkGodot, func(o *BuildOptions) { o.GodotDir = godotDir })
}
