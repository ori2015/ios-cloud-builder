package projectdetect

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// HasContainer reports whether dir directly holds a usable Xcode workspace or
// an Xcode project.
func HasContainer(dir string) bool {
	workspaces, _ := filepath.Glob(filepath.Join(dir, "*.xcworkspace"))
	for _, w := range workspaces {
		if UsableWorkspace(w) {
			return true
		}
	}
	projects, _ := filepath.Glob(filepath.Join(dir, "*.xcodeproj"))
	return len(projects) > 0
}

// HasXcodeGenManifest reports whether dir holds an XcodeGen manifest that
// defines targets (XcodeGen can then create the project during the build).
func HasXcodeGenManifest(dir string) bool {
	for _, name := range []string{"project.yml", "project.yaml"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err == nil && len(data) <= 2*1024*1024 && xcodegenTargetRe.Match(data) {
			return true
		}
	}
	return false
}

// UsableWorkspace reports whether an .xcworkspace is a real workspace: it must
// list at least one project and every non-Pods project it lists must exist. An
// empty stub, or one that points at an .xcodeproj that is not in git, is not
// usable; XcodeGen should generate the project instead.
func UsableWorkspace(workspace string) bool {
	data, err := os.ReadFile(filepath.Join(workspace, "contents.xcworkspacedata"))
	if err != nil || len(data) > 1024*1024 {
		return false
	}
	base := filepath.Dir(workspace)
	listed := 0
	for _, location := range workspaceLocations(string(data)) {
		listed++
		kind, rel, ok := strings.Cut(location, ":")
		if !ok {
			return false
		}
		if kind == "absolute" || strings.HasPrefix(rel, "Pods/") {
			continue // not checkable in the repository / created by pod install
		}
		target := filepath.Join(base, filepath.FromSlash(rel))
		if _, err := os.Stat(target); err != nil {
			return false
		}
	}
	return listed > 0
}

// workspaceLocations extracts the location="..." attribute of each FileRef.
func workspaceLocations(data string) []string {
	var out []string
	const key = `location = "`
	for {
		i := strings.Index(data, key)
		if i < 0 {
			return out
		}
		data = data[i+len(key):]
		j := strings.IndexByte(data, '"')
		if j < 0 {
			return out
		}
		out = append(out, data[:j])
		data = data[j+1:]
	}
}

func layoutFor(root string, c Candidate) (*Layout, error) {
	appDir := filepath.Join(root, filepath.FromSlash(c.Path))
	layout := &Layout{AppPath: c.Path, Kind: c.Kind}
	join := func(rel string) string {
		if rel == "." {
			return c.Path
		}
		return path.Join(c.Path, rel)
	}
	has := func(rel string) bool {
		d := filepath.Join(appDir, filepath.FromSlash(rel))
		return HasContainer(d) || HasXcodeGenManifest(d)
	}
	switch c.Kind {
	case KindFlutter:
		layout.IOSPath = join("ios")
	case KindKMP:
		for _, rel := range []string{"iosApp", "ios"} {
			if has(rel) {
				layout.IOSPath = join(rel)
				return layout, nil
			}
		}
		return nil, fmt.Errorf("%s: Kotlin Multiplatform app has no iosApp/ or ios/ Xcode project; pass --ios-path", c.Path)
	case KindNode:
		for _, rel := range []string{"ios", "ios/App", "platforms/ios"} {
			if has(rel) {
				layout.IOSPath = join(rel)
				return layout, nil
			}
		}
		// Managed Expo and Cordova keep no iOS folder in git; the build creates it.
		rel, generated := generatedIOSDir(appDir)
		if !generated {
			return nil, fmt.Errorf("%s: JavaScript app has no committed ios/ project and cannot generate one; commit ios/ (for Capacitor run `npx cap add ios`) or pass --ios-path", c.Path)
		}
		layout.IOSPath, layout.Generated = join(rel), true
	default:
		layout.IOSPath = join(".")
		if !has(".") {
			if rel, ok := firstIOSDirBelow(appDir); ok {
				layout.IOSPath = join(rel)
			}
		}
	}
	return layout, nil
}

// generatedIOSDir returns where a build that generates the iOS project puts it.
func generatedIOSDir(appDir string) (string, bool) {
	pkg, _ := os.ReadFile(filepath.Join(appDir, "package.json"))
	switch {
	case strings.Contains(string(pkg), `"expo"`):
		return "ios", true
	case strings.Contains(string(pkg), `"cordova"`) || isCordovaConfig(appDir):
		return "platforms/ios", true
	}
	return "", false
}

// firstIOSDirBelow finds a single immediate subdirectory holding an Xcode container.
func firstIOSDirBelow(appDir string) (string, bool) {
	entries, err := os.ReadDir(appDir)
	if err != nil {
		return "", false
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && !skipDirs[e.Name()] && !strings.HasPrefix(e.Name(), ".") && !hasSkipSuffix(e.Name()) &&
			(HasContainer(filepath.Join(appDir, e.Name())) || HasXcodeGenManifest(filepath.Join(appDir, e.Name()))) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 1 {
		return names[0], true
	}
	return "", false
}
