package runner

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/MobAI-App/ios-builder/internal/projectdetect"
)

// DetectFrameworkAt is DetectFramework for an app that lives in appPath, a
// repository-relative folder (empty or "." for the checkout root).
func DetectFrameworkAt(sourceRoot, appPath, hint string) (string, error) {
	if appPath != "" && appPath != "." {
		if err := validateRelativePath(appPath); err != nil {
			return "", os.ErrInvalid
		}
		if real, err := filepath.EvalSymlinks(sourceRoot); err == nil {
			sourceRoot = real
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
	framework, err := projectdetect.DetectFramework(sourceRoot)
	if errors.Is(err, projectdetect.ErrUnsupportedFramework) {
		return "", err
	}
	if err != nil {
		// Nothing identifiable: keep the long-standing behaviour of building the
		// directory as a native Xcode project, which reports a precise error if it is not one.
		return FrameworkNative, nil
	}
	return framework, nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
