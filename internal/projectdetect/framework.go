package projectdetect

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Framework names. They are the values stored in the project registry and
// accepted by the runner.
const (
	FrameworkNative       = "native"
	FrameworkFlutter      = "flutter"
	FrameworkReactNative  = "react-native"
	FrameworkExpo         = "expo"
	FrameworkKMP          = "kmp"
	FrameworkCordova      = "cordova"
	FrameworkIonic        = "ionic" // Ionic or plain Capacitor
	FrameworkTauri        = "tauri" // Tauri 2 mobile
	FrameworkNativeScript = "nativescript"
	FrameworkSparkling    = "sparkling" // Lynx app framework with a committed ios/ project
	FrameworkMAUI         = "maui"      // .NET MAUI
)

// ErrUnsupportedFramework means the project is a recognised engine that this
// builder does not build. The wrapped message is generic: it names no project data.
var ErrUnsupportedFramework = errors.New("unsupported framework")

// ErrUnrecognized means nothing in the directory identifies a supported framework
// or an Xcode project.
var ErrUnrecognized = errors.New("no supported iOS project recognised")

var (
	flutterTopLevelRe = regexp.MustCompile(`(?m)^flutter:`)
	flutterSDKRe      = regexp.MustCompile(`(?m)^\s+sdk:\s*flutter\b`)
	expoDepRe         = regexp.MustCompile(`"expo"\s*:`)
	reactNativeDepRe  = regexp.MustCompile(`"react-native"\s*:`)
	sparklingDepRe    = regexp.MustCompile(`"sparkling-app-cli"\s*:`)
	nativeScriptDepRe = regexp.MustCompile(`"(@nativescript/core|nativescript)"\s*:`)
	ionicDepRe        = regexp.MustCompile(`"@ionic/|"ionic"\s*:`)
	capacitorIOSRe    = regexp.MustCompile(`"@capacitor/ios"`)
	cordovaDepRe      = regexp.MustCompile(`"cordova"\s*:`)
	// KMPPluginRe matches a declaration of the Kotlin Multiplatform Gradle plugin
	// in the Kotlin DSL, Groovy or plugin-id form. The CLI and both workflow
	// templates must agree with it.
	KMPPluginRe = regexp.MustCompile(`kotlin\("multiplatform"\)|org\.jetbrains\.kotlin\.multiplatform|id\(["']org\.jetbrains\.kotlin\.multiplatform["']\)`)
)

// HasTauriConfig reports whether dir holds a Tauri project: a src-tauri folder
// with tauri.conf.json, tauri.conf.json5 or Tauri.toml.
func HasTauriConfig(dir string) bool {
	for _, name := range []string{"tauri.conf.json", "tauri.conf.json5", "Tauri.toml"} {
		if fileExists(filepath.Join(dir, "src-tauri", name)) {
			return true
		}
	}
	return false
}

// IsFlutterPubspec reports whether pubspec contents describe a Flutter app.
func IsFlutterPubspec(pubspec []byte) bool {
	return flutterTopLevelRe.Match(pubspec) || flutterSDKRe.Match(pubspec)
}

// IsCapacitorProject reports whether root has a Capacitor configuration file.
func IsCapacitorProject(root string) bool {
	for _, name := range []string{"capacitor.config.ts", "capacitor.config.js", "capacitor.config.json"} {
		if fileExists(filepath.Join(root, name)) {
			return true
		}
	}
	return false
}

// DetectFramework identifies the framework of the app in appRoot from file
// contents only. A managed Expo app (no committed ios/ folder) is "expo"; an
// Expo app that already has an Xcode project is built like React Native.
func DetectFramework(appRoot string) (string, error) {
	if pubspec, err := os.ReadFile(filepath.Join(appRoot, "pubspec.yaml")); err == nil && IsFlutterPubspec(pubspec) {
		return FrameworkFlutter, nil
	}
	if HasTauriConfig(appRoot) {
		return FrameworkTauri, nil
	}
	pkg, _ := os.ReadFile(filepath.Join(appRoot, "package.json"))
	switch {
	case sparklingDepRe.Match(pkg):
		return FrameworkSparkling, nil
	case nativeScriptDepRe.Match(pkg) || HasNativeScriptConfig(appRoot):
		return FrameworkNativeScript, nil
	case expoDepRe.Match(pkg):
		if hasIOSContainerBelow(appRoot, "ios") {
			return FrameworkReactNative, nil
		}
		return FrameworkExpo, nil
	case reactNativeDepRe.Match(pkg):
		return FrameworkReactNative, nil
	case ionicDepRe.Match(pkg) || (capacitorIOSRe.Match(pkg) && IsCapacitorProject(appRoot)):
		return FrameworkIonic, nil
	case cordovaDepRe.Match(pkg) || isCordovaConfig(appRoot):
		return FrameworkCordova, nil
	}
	if _, _, ok := MAUIProject(appRoot); ok {
		return FrameworkMAUI, nil
	}
	if engine, reason := UnsupportedEngine(appRoot); engine != "" {
		return "", fmt.Errorf("%w: %s", ErrUnsupportedFramework, reason)
	}
	if hasKMPPlugin(appRoot) {
		return FrameworkKMP, nil
	}
	if candidates, err := FindAppRoots(appRoot); err == nil && len(candidates) > 0 {
		return FrameworkNative, nil
	}
	if fileExists(filepath.Join(appRoot, "Package.swift")) {
		return "", fmt.Errorf("%w: found Package.swift but no Xcode project; a Swift package builds libraries, not an installable app, so add an app project (an .xcodeproj, or a project.yml for XcodeGen)", ErrUnrecognized)
	}
	return "", fmt.Errorf("%w in %s: found none of pubspec.yaml (Flutter), package.json (Expo, React Native, Capacitor, Ionic, Cordova), a Kotlin Multiplatform Gradle build, an Xcode project/workspace or project.yml; pass --app-path to point at the app folder", ErrUnrecognized, filepath.Base(appRoot))
}

func hasKMPPlugin(root string) bool {
	found := false
	_ = filepath.WalkDir(root, func(p string, entry fs.DirEntry, err error) error {
		if err != nil || found {
			return filepath.SkipAll
		}
		if entry.IsDir() {
			if p != root && (strings.HasPrefix(entry.Name(), ".") || skipDirs[entry.Name()]) {
				return filepath.SkipDir
			}
			return nil
		}
		switch entry.Name() {
		case "build.gradle", "build.gradle.kts", "libs.versions.toml":
			if contents, readErr := os.ReadFile(p); readErr == nil && bytes.Contains(contents, []byte("multiplatform")) && KMPPluginRe.Match(contents) {
				found = true
				return filepath.SkipAll
			}
		}
		return nil
	})
	return found
}

var (
	mauiTFMRe  = regexp.MustCompile(`net\d+\.\d+-ios`)
	godotIOSRe = regexp.MustCompile(`(?m)^platform="iOS"`)
)

// UnsupportedEngine recognises engines whose iOS build is not implemented:
// Godot and Unity. It returns the engine name and a one-sentence
// reason that says what is missing and what to do instead.
func UnsupportedEngine(dir string) (engine, reason string) {
	const exportAdvice = " Export the iOS Xcode project from the engine, commit it, and register that folder with --app-path; it then builds as a native project."
	if fileExists(filepath.Join(dir, "ProjectSettings", "ProjectVersion.txt")) {
		return "unity", "Unity is recognised but not built: generating the Xcode project needs the Unity editor and a license on the runner, which this builder does not provide." + exportAdvice
	}
	if fileExists(filepath.Join(dir, "project.godot")) {
		presets, _ := os.ReadFile(filepath.Join(dir, "export_presets.cfg"))
		if godotIOSRe.Match(presets) {
			return "godot", "Godot is recognised but not built: exporting needs the Godot editor and export templates on the runner, which are not installed or verified here." + exportAdvice
		}
	}
	return "", ""
}

// MAUIProject finds the .NET MAUI project in dir: a *.csproj that sets UseMaui
// and targets iOS. It returns the file name and the iOS target framework
// moniker (for example net8.0-ios).
func MAUIProject(dir string) (csproj, tfm string, ok bool) {
	matches, _ := filepath.Glob(filepath.Join(dir, "*.csproj"))
	sort.Strings(matches)
	for _, match := range matches {
		data, err := os.ReadFile(match)
		if err != nil || len(data) > 1<<20 || !strings.Contains(string(data), "<UseMaui>true</UseMaui>") {
			continue
		}
		if found := mauiTFMRe.Find(data); found != nil {
			return filepath.Base(match), string(found), true
		}
	}
	return "", "", false
}
