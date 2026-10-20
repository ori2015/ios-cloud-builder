package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/MobAI-App/ios-builder/internal/projectdetect"
)

var mauiXcodeMismatchRe = regexp.MustCompile(`requires Xcode ([0-9][0-9.]*)\. The current version of Xcode is ([0-9][0-9.]*)`)

// buildMAUI builds a .NET MAUI iOS app without code signing. The commands were
// proven on a macOS runner against a generated MAUI app:
//
//	dotnet workload restore <csproj>
//	dotnet publish <csproj> -f <net*-ios> -c Release -r ios-arm64 \
//	    -p:EnableCodeSigning=false -p:BuildIpa=true
//
// EnableCodeSigning is a documented dotnet/macios build property. The IPA that
// publish packages is unsigned. The build is always Release (publish builds the
// IPA from the Release configuration).
func buildMAUI(run executor, appRoot string, options *BuildOptions) error {
	csproj, tfm, ok := projectdetect.MAUIProject(appRoot)
	if !ok {
		return fmt.Errorf("no .NET MAUI project (a .csproj with <UseMaui>true</UseMaui> and a net*-ios target) in the app folder; pass --app-path for the folder that holds it")
	}
	if err := run.run(appRoot, "dotnet", "workload", "restore", csproj); err != nil {
		return err
	}
	output, err := run.runCollect(appRoot, "dotnet", "publish", csproj, "-f", tfm, "-c", "Release", "-r", "ios-arm64",
		"-p:EnableCodeSigning=false", "-p:BuildIpa=true")
	if err != nil {
		if hint := explainMAUIFailure(string(output)); hint != "" {
			return fmt.Errorf("%s: %w", hint, err)
		}
		return err
	}
	ipa, err := findMAUIIPA(appRoot)
	if err != nil {
		return err
	}
	return finishPrebuiltIPA(ipa, options)
}

// explainMAUIFailure turns the .NET iOS workload's Xcode version check into an
// action. The latest workload of a .NET release usually targets a newer Xcode
// than the runner image has installed.
func explainMAUIFailure(output string) string {
	m := mauiXcodeMismatchRe.FindStringSubmatch(output)
	if m == nil {
		return ""
	}
	return fmt.Sprintf("the .NET iOS workload needs Xcode %s but the build uses Xcode %s; add a .xcode-version file containing the Xcode version this app builds with if the runner has it, "+
		"or pin an older workload set (global.json \"workloadVersion\") or the net8.0-ios target, whose workload matches Xcode 16", m[1], m[2])
}

func findMAUIIPA(appRoot string) (string, error) {
	var found []string
	_ = filepath.WalkDir(filepath.Join(appRoot, "bin"), func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && strings.HasSuffix(entry.Name(), ".ipa") && filepath.Base(filepath.Dir(path)) == "publish" {
			found = append(found, path)
		}
		return nil
	})
	sort.Strings(found)
	if len(found) == 0 {
		return "", fmt.Errorf("dotnet publish finished but left no .ipa under bin/**/publish; check the log above")
	}
	return found[0], nil
}
