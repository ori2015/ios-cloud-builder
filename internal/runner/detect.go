package runner

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
)

// DetectFrameworkAt is DetectFramework for an app that lives in appPath, a
// repository-relative folder (empty or "." for the checkout root).
func DetectFrameworkAt(sourceRoot, appPath, hint string) (string, error) {
	if appPath != "" && appPath != "." {
		if err := validateRelativePath(appPath); err != nil {
			return "", os.ErrInvalid
		}
		resolved, err := filepath.EvalSymlinks(filepath.Join(sourceRoot, filepath.FromSlash(appPath)))
		if err != nil || !pathWithin(sourceRoot, resolved) {
			return "", os.ErrNotExist
		}
		sourceRoot = resolved
	}
	return DetectFramework(sourceRoot, hint)
}

// DetectFramework inspects manifests without executing private project code.
func DetectFramework(sourceRoot, hint string) (string, error) {
	if !validFramework(hint) {
		return "", os.ErrInvalid
	}
	if hint != FrameworkAuto {
		return hint, nil
	}
	if exists(filepath.Join(sourceRoot, "pubspec.yaml")) {
		return FrameworkFlutter, nil
	}
	pkg, _ := os.ReadFile(filepath.Join(sourceRoot, "package.json"))
	switch {
	case bytes.Contains(pkg, []byte(`"expo"`)):
		return FrameworkExpo, nil
	case bytes.Contains(pkg, []byte(`"react-native"`)):
		return FrameworkReactNative, nil
	case bytes.Contains(pkg, []byte(`"@ionic/`)) || bytes.Contains(pkg, []byte(`"ionic"`)) ||
		(bytes.Contains(pkg, []byte(`"@capacitor/ios"`)) && isCapacitorProject(sourceRoot)):
		return FrameworkIonic, nil
	case bytes.Contains(pkg, []byte(`"cordova"`)) || exists(filepath.Join(sourceRoot, "config.xml")):
		return FrameworkCordova, nil
	}
	foundKMP := false
	_ = filepath.WalkDir(sourceRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil || foundKMP {
			return filepath.SkipAll
		}
		if entry.IsDir() {
			if path != sourceRoot && (strings.HasPrefix(entry.Name(), ".") || entry.Name() == "node_modules" || entry.Name() == "Pods") {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() != "build.gradle" && entry.Name() != "build.gradle.kts" && entry.Name() != "libs.versions.toml" {
			return nil
		}
		contents, readErr := os.ReadFile(path)
		if readErr == nil && (bytes.Contains(contents, []byte("kotlin(\"multiplatform\")")) ||
			bytes.Contains(contents, []byte("org.jetbrains.kotlin.multiplatform"))) {
			foundKMP = true
			return filepath.SkipAll
		}
		return nil
	})
	if foundKMP {
		return FrameworkKMP, nil
	}
	return FrameworkNative, nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
