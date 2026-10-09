package runner

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var (
	pubSDKVersionRe = regexp.MustCompile(`current Flutter SDK version is ([0-9][^\s]*?)\.?(?:\s|$)`)
	lockPackageRe   = regexp.MustCompile(`(?m)^  ([a-z0-9_]+):\n(?:    .*\n)*?    version: "([^"]+)"`)
)

// explainPubGetFailure turns a `flutter pub get` dependency-solving failure into
// the actionable cause: the packages pubspec.lock pins that the log names, and
// the SDK that was used. It returns "" for failures that are not solver
// conflicts (network, missing package), which need no extra explanation.
func explainPubGetFailure(output, lock string) string {
	if !strings.Contains(output, "version solving failed") && !strings.Contains(output, "from sdk") {
		return ""
	}
	sdk := "the latest stable"
	if m := pubSDKVersionRe.FindStringSubmatch(output); m != nil {
		sdk = m[1]
	}
	var pinned []string
	for _, m := range lockPackageRe.FindAllStringSubmatch(lock, -1) {
		if regexp.MustCompile(`\b` + regexp.QuoteMeta(m[1]) + `\b`).MatchString(output) {
			pinned = append(pinned, m[1]+" "+m[2])
		}
	}
	sort.Strings(pinned)
	detail := "pubspec.lock was not found"
	if lock != "" {
		detail = "pubspec.lock pins none of the packages named above"
	}
	if len(pinned) > 0 {
		detail = "pubspec.lock pins " + strings.Join(pinned, ", ")
	}
	return fmt.Sprintf("flutter pub get could not resolve dependencies against Flutter %s: %s. "+
		"The lock was resolved with a different Flutter release; run `flutter --version` where the app builds locally and commit that version "+
		"(x.y.z) as .flutter-version in the app folder, or run `flutter pub upgrade` and commit the new pubspec.lock", sdk, detail)
}
