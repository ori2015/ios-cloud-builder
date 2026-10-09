// Package projectdetect finds where an iOS app lives inside a repository and
// which Xcode container and scheme to build, from file contents alone. It never
// executes project code, so both the CLI and the trusted runner can use it.
package projectdetect

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Kind classifies what marked a directory as an app root.
type Kind string

const (
	KindFlutter  Kind = "flutter"
	KindNode     Kind = "node" // Expo, React Native, Capacitor, Ionic or Cordova
	KindKMP      Kind = "kmp"
	KindXcode    Kind = "xcode"
	KindXcodeGen Kind = "xcodegen"
	KindTauri    Kind = "tauri"
	// KindUnsupported marks engines that are recognised but not built (see UnsupportedEngine).
	KindUnsupported Kind = "unsupported"
)

// MaxDepth bounds the repository scan; app roots deeper than this need --app-path.
const MaxDepth = 4

// Candidate is a directory that looks like the root of one iOS app.
type Candidate struct {
	Path string // slash-separated, relative to the repository root; "." is the root
	Kind Kind
}

var skipDirs = map[string]bool{
	"node_modules": true, "Pods": true, ".git": true, "build": true, ".dart_tool": true,
	".gradle": true, "DerivedData": true, "Carthage": true, "vendor": true, ".build": true,
	".symlinks": true, "dist": true, ".idea": true,
}

// skipSuffixes are bundle-like directories whose contents are never app roots.
var skipSuffixes = []string{".xcodeproj", ".xcworkspace", ".app", ".framework", ".xcframework", ".xcassets", ".bundle", ".lproj", ".playground"}

var (
	nodeMarkerRe     = regexp.MustCompile(`"(expo|react-native|cordova|ionic|nativescript|sparkling-app-cli)"|"@(capacitor|ionic|nativescript)/`)
	xcodegenTargetRe = regexp.MustCompile(`(?m)^targets:`)
)

// FindAppRoots scans root (to MaxDepth, skipping dependency and build
// directories) and returns every directory that looks like an iOS app root.
// Native Xcode directories that sit inside a framework app (ios/ under a
// Flutter or Node app) are folded into that app rather than reported twice.
func FindAppRoots(root string) ([]Candidate, error) {
	var found []Candidate
	err := filepath.WalkDir(root, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			return fs.SkipDir
		}
		if !entry.IsDir() {
			return nil
		}
		relative, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		relative = filepath.ToSlash(relative)
		if p != root {
			if skipDirs[entry.Name()] || strings.HasPrefix(entry.Name(), ".") || hasSkipSuffix(entry.Name()) {
				return fs.SkipDir
			}
			if strings.Count(relative, "/") >= MaxDepth {
				return fs.SkipDir
			}
		}
		if kind, ok := classify(p); ok {
			found = append(found, Candidate{Path: relative, Kind: kind})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return foldNested(found), nil
}

func hasSkipSuffix(name string) bool {
	for _, suffix := range skipSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// classify decides whether dir is an app root, preferring the framework that
// generates or wraps the Xcode project over the bare Xcode project itself.
func classify(dir string) (Kind, bool) {
	if pubspec, err := os.ReadFile(filepath.Join(dir, "pubspec.yaml")); err == nil &&
		IsFlutterPubspec(pubspec) && hasIOSContainerBelow(dir, "ios") {
		return KindFlutter, true
	}
	if HasTauriConfig(dir) {
		return KindTauri, true
	}
	if HasNativeScriptConfig(dir) {
		return KindNode, true
	}
	if engine, _ := UnsupportedEngine(dir); engine != "" {
		return KindUnsupported, true
	}
	if pkg, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil && nodeMarkerRe.Match(pkg) {
		return KindNode, true
	}
	if isCordovaConfig(dir) {
		return KindNode, true
	}
	if hasGradleSettings(dir) && (hasIOSContainerBelow(dir, "iosApp") || hasIOSContainerBelow(dir, "ios")) {
		return KindKMP, true
	}
	if HasContainer(dir) {
		return KindXcode, true
	}
	if HasXcodeGenManifest(dir) {
		return KindXcodeGen, true
	}
	return "", false
}

// HasNativeScriptConfig reports whether dir has a NativeScript config file.
func HasNativeScriptConfig(dir string) bool {
	for _, name := range []string{"nativescript.config.ts", "nativescript.config.js", "nativescript.config.json"} {
		if fileExists(filepath.Join(dir, name)) {
			return true
		}
	}
	return false
}

func isCordovaConfig(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "config.xml"))
	return err == nil && strings.Contains(string(data), "<widget")
}

func hasGradleSettings(dir string) bool {
	return fileExists(filepath.Join(dir, "settings.gradle")) || fileExists(filepath.Join(dir, "settings.gradle.kts"))
}

// hasIOSContainerBelow reports whether dir/sub holds an Xcode project, a
// workspace or an XcodeGen manifest.
func hasIOSContainerBelow(dir, sub string) bool {
	d := filepath.Join(dir, filepath.FromSlash(sub))
	return HasContainer(d) || HasXcodeGenManifest(d)
}

// foldNested drops candidates that live inside a framework app root, since the
// Xcode project under ios/ belongs to that app and is not a second app.
func foldNested(found []Candidate) []Candidate {
	sort.Slice(found, func(i, j int) bool { return found[i].Path < found[j].Path })
	var kept []Candidate
	for _, c := range found {
		shadowed := false
		for _, other := range found {
			if other.Path == c.Path || (other.Kind != KindFlutter && other.Kind != KindNode && other.Kind != KindKMP && other.Kind != KindTauri) {
				continue
			}
			if isUnder(other.Path, c.Path) {
				shadowed = true
				break
			}
		}
		if !shadowed {
			kept = append(kept, c)
		}
	}
	return kept
}

func isUnder(parent, child string) bool {
	if parent == "." {
		return child != "."
	}
	return strings.HasPrefix(child, parent+"/")
}

// Layout is where an app lives and where its Xcode project is (or will be, for
// frameworks that generate it during the build).
type Layout struct {
	AppPath   string // repository-relative app root
	IOSPath   string // repository-relative directory holding the Xcode container
	Kind      Kind
	Generated bool // the iOS directory does not exist yet; the build creates it
}

// ErrNoApp and ErrAmbiguous let callers tell "found nothing" from "found several".
var (
	ErrNoApp     = errors.New("no iOS app found")
	ErrAmbiguous = errors.New("more than one iOS app found")
)

// Resolve picks the app root and iOS path for the repository at root. A
// non-empty appPath overrides the scan.
func Resolve(root, appPath string) (*Layout, error) {
	if appPath != "" {
		clean, err := cleanRelative(appPath)
		if err != nil {
			return nil, fmt.Errorf("--app-path %q: %w", appPath, err)
		}
		info, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(clean)))
		if statErr != nil || !info.IsDir() {
			return nil, fmt.Errorf("--app-path %q is not a directory in this repository; pass the folder that holds your app's pubspec.yaml, package.json or Xcode project", appPath)
		}
		kind, ok := classify(filepath.Join(root, filepath.FromSlash(clean)))
		if !ok {
			return nil, fmt.Errorf("%s has no pubspec.yaml, package.json, Xcode project or project.yml; pass --app-path for the folder that does", clean)
		}
		return layoutFor(root, Candidate{Path: clean, Kind: kind})
	}
	candidates, err := FindAppRoots(root)
	if err != nil {
		return nil, fmt.Errorf("scan repository: %w", err)
	}
	switch len(candidates) {
	case 0:
		if fileExists(filepath.Join(root, "Package.swift")) {
			return nil, fmt.Errorf("%w: found Package.swift but no Xcode project; a Swift package builds libraries, not an installable app, so add an app project (an .xcodeproj, or a project.yml for XcodeGen)", ErrNoApp)
		}
		return nil, fmt.Errorf("%w: looked for pubspec.yaml (Flutter), package.json (Expo, React Native, Capacitor, Ionic), config.xml (Cordova), an Xcode project or workspace, or project.yml (XcodeGen) within %d folders; pass --app-path <folder> if your app is deeper", ErrNoApp, MaxDepth)
	case 1:
		return layoutFor(root, candidates[0])
	default:
		names := make([]string, len(candidates))
		for i, c := range candidates {
			names[i] = c.Path + " (" + string(c.Kind) + ")"
		}
		return nil, fmt.Errorf("%w: %s; pass --app-path <folder> to choose one", ErrAmbiguous, strings.Join(names, ", "))
	}
}

func cleanRelative(value string) (string, error) {
	if value == "" {
		return ".", nil
	}
	if filepath.IsAbs(value) || strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\\\x00\r\n") {
		return "", errors.New("must be a relative path inside the repository using forward slashes")
	}
	clean := path.Clean(value)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("must stay inside the repository")
	}
	return clean, nil
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}
