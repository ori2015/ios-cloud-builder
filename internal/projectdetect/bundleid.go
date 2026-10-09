package projectdetect

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	pbxprojBlockRe   = regexp.MustCompile(`(?s)buildSettings = \{(.*?)\n\t*\};\s*name = ([^;]+);`)
	pbxprojBundleRe  = regexp.MustCompile(`PRODUCT_BUNDLE_IDENTIFIER = ("[^"]+"|[^;\s]+);`)
	xcodegenBundleRe = regexp.MustCompile(`(?m)^\s*PRODUCT_BUNDLE_IDENTIFIER:\s*["']?([^\s"'#]+)`)
	capacitorAppIDRe = regexp.MustCompile(`appId\s*:\s*['"]([^'"]+)['"]`)
)

// BundleID reads the application's bundle identifier from the project without
// running it. It returns "" and a reason when the identifier cannot be
// determined unambiguously (build variables, several unrelated targets); the
// caller then leaves the project unpinned or asks for --bundle-id. A wrong guess
// would make the runner reject the build, so it never guesses.
func BundleID(root string, layout *Layout, configuration string) (id, reason string) {
	if layout == nil {
		return "", "no project layout"
	}
	appDir := filepath.Join(root, filepath.FromSlash(layout.AppPath))
	iosDir := filepath.Join(root, filepath.FromSlash(layout.IOSPath))

	// What xcodebuild will read wins over configuration that generates it.
	if matches, _ := filepath.Glob(filepath.Join(iosDir, "*.xcodeproj")); len(matches) > 0 {
		sort.Strings(matches)
		var ids []string
		for _, project := range matches {
			if filepath.Base(project) == "Pods.xcodeproj" {
				continue
			}
			data, err := os.ReadFile(filepath.Join(project, "project.pbxproj"))
			if err != nil {
				continue
			}
			found, why := pbxprojBundleIDs(string(data), configuration)
			if why != "" {
				return "", why
			}
			ids = append(ids, found...)
		}
		if len(ids) > 0 {
			return pickAppID(ids)
		}
	}
	if data, ok := firstFile(iosDir, "project.yml", "project.yaml"); ok {
		var ids []string
		for _, m := range xcodegenBundleRe.FindAllStringSubmatch(data, -1) {
			ids = append(ids, m[1])
		}
		if len(ids) > 0 {
			literal, why := onlyLiteral(ids)
			if why != "" {
				return "", why
			}
			return pickAppID(literal)
		}
	}
	if layout.Kind == KindNode {
		if id := expoBundleID(appDir); id != "" {
			return validOrReason(id)
		}
		if id := capacitorBundleID(appDir); id != "" {
			return validOrReason(id)
		}
	}
	return "", "no literal PRODUCT_BUNDLE_IDENTIFIER found in the project"
}

func validOrReason(id string) (string, string) {
	if strings.Contains(id, "$") {
		return "", "the bundle identifier is built from variables"
	}
	return id, ""
}

func pbxprojBundleIDs(pbxproj, configuration string) ([]string, string) {
	var forConfig, anyConfig []string
	for _, block := range pbxprojBlockRe.FindAllStringSubmatch(pbxproj, -1) {
		name := strings.Trim(strings.TrimSpace(block[2]), `"`)
		for _, m := range pbxprojBundleRe.FindAllStringSubmatch(block[1], -1) {
			id := strings.Trim(m[1], `"`)
			anyConfig = append(anyConfig, id)
			if name == configuration {
				forConfig = append(forConfig, id)
			}
		}
	}
	chosen := forConfig
	if len(chosen) == 0 {
		chosen = anyConfig
	}
	return onlyLiteral(chosen)
}

func onlyLiteral(ids []string) ([]string, string) {
	for _, id := range ids {
		if strings.Contains(id, "$") {
			return nil, "the bundle identifier is built from build variables (" + id + "); pass --bundle-id"
		}
	}
	return ids, ""
}

// pickAppID returns the application's identifier from all target identifiers: the
// shortest one, provided every other identifier is it or a dotted extension of
// it (extensions, tests, watch apps). Anything else is ambiguous.
func pickAppID(ids []string) (string, string) {
	if len(ids) == 0 {
		return "", "no bundle identifier found"
	}
	shortest := ids[0]
	for _, id := range ids {
		if len(id) < len(shortest) {
			shortest = id
		}
	}
	for _, id := range ids {
		if id != shortest && !strings.HasPrefix(id, shortest+".") {
			return "", fmt.Sprintf("the project has unrelated bundle identifiers (%s and %s); pass --bundle-id", shortest, id)
		}
	}
	return shortest, ""
}

func firstFile(dir string, names ...string) (string, bool) {
	for _, name := range names {
		if data, err := os.ReadFile(filepath.Join(dir, name)); err == nil && len(data) <= 2*1024*1024 {
			return string(data), true
		}
	}
	return "", false
}

func expoBundleID(appDir string) string {
	for _, name := range []string{"app.json", "app.config.json"} {
		data, err := os.ReadFile(filepath.Join(appDir, name))
		if err != nil {
			continue
		}
		var cfg struct {
			Expo struct {
				IOS struct {
					BundleIdentifier string `json:"bundleIdentifier"`
				} `json:"ios"`
			} `json:"expo"`
		}
		if json.Unmarshal(data, &cfg) == nil && cfg.Expo.IOS.BundleIdentifier != "" {
			return cfg.Expo.IOS.BundleIdentifier
		}
	}
	return ""
}

func capacitorBundleID(appDir string) string {
	for _, name := range []string{"capacitor.config.json", "capacitor.config.ts", "capacitor.config.js"} {
		data, err := os.ReadFile(filepath.Join(appDir, name))
		if err != nil {
			continue
		}
		if m := capacitorAppIDRe.FindSubmatch(data); m != nil {
			return string(m[1])
		}
		var cfg struct {
			AppID string `json:"appId"`
		}
		if json.Unmarshal(data, &cfg) == nil && cfg.AppID != "" {
			return cfg.AppID
		}
	}
	return ""
}
