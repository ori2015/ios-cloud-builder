package projectdetect

import (
	"fmt"
	"strings"
)

// excludedSchemeSuffixes mark schemes that build a test bundle or an embedded
// extension rather than the app itself.
var excludedSchemeSuffixes = []string{
	"Tests", "UITests", "Test", "Extension", "Extensions", "Widget", "WidgetExtension",
	"Intents", "IntentsUI", "Clip", "AppClip", "WatchKit", "WatchKitExtension", "Watch", "Stickers",
}

// PickScheme chooses the application scheme from an `xcodebuild -list` result.
// containerName is the workspace or project name without its extension. It
// returns an error naming the candidates when the choice is still ambiguous, so
// the caller can ask for --scheme instead of building the wrong target.
func PickScheme(schemes []string, containerName string) (string, error) {
	// A scheme named exactly like the workspace or project is the app, even when
	// its name happens to end in a suffix such as "Watch".
	for _, scheme := range schemes {
		if scheme != "" && scheme == containerName && !strings.HasPrefix(scheme, "Pods") {
			return scheme, nil
		}
	}
	var apps []string
	for _, scheme := range schemes {
		if scheme == "" || strings.HasPrefix(scheme, "Pods-") || scheme == "Pods" || isExcludedScheme(scheme) {
			continue
		}
		apps = append(apps, scheme)
	}
	switch len(apps) {
	case 0:
		return "", fmt.Errorf("no application scheme found among %d schemes; share the app scheme in Xcode (Product > Scheme > Manage Schemes > Shared) or set ios.scheme in builder.json", len(schemes))
	case 1:
		return apps[0], nil
	default:
		return "", fmt.Errorf("more than one application scheme found: %s; set ios.scheme in builder.json or pass --scheme", strings.Join(apps, ", "))
	}
}

func isExcludedScheme(scheme string) bool {
	for _, suffix := range excludedSchemeSuffixes {
		if strings.HasSuffix(scheme, suffix) && len(scheme) > len(suffix) {
			return true
		}
	}
	return false
}
