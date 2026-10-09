package runner

import (
	"strings"
	"testing"
)

const sparklingPackage = `{"scripts":{"build":"sparkling-app-cli build --copy"},"devDependencies":{"sparkling-app-cli":"~2.0.1"}}`

func sparklingOptions(root, output string) *BuildOptions {
	return &BuildOptions{
		Framework: FrameworkSparkling, IOSPath: "ios", Configuration: "Debug", SourceRoot: root,
		LogPath: output + "/private-output/build.log", IPAPath: output + "/private-output/App.ipa",
	}
}

func TestSparklingBuildsLynxBundlesThenTheCommittedXcodeProject(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "package.json", sparklingPackage)
	writeFile(t, root, "ios/Sample.xcodeproj/project.pbxproj", "{}")
	writeFile(t, root, "ios/Sample.xcworkspace/contents.xcworkspacedata",
		`<Workspace version = "1.0"><FileRef location = "group:Sample.xcodeproj"></FileRef></Workspace>`)
	writeFile(t, root, "ios/Podfile", "platform :ios, '12.0'\n")
	writeFile(t, root, ".git/config", "[core]\n")
	calls := installFakeTools(t, map[string]string{
		"npm":        "",
		"pod":        "",
		"xcodebuild": "case \"$*\" in *-list*) echo '{\"project\":{\"schemes\":[\"Sample\",\"SampleTests\",\"SampleUITests\"]},\"workspace\":{\"schemes\":[\"Lynx\",\"Sample\",\"SDWebImage\"]}}';; esac",
	})
	_ = BuildUnsigned(t.Context(), sparklingOptions(root, t.TempDir())) // packaging needs macOS ditto
	got := strings.Join(readCalls(t, calls), "\n")
	order := []string{"npm install", "npm run build", "pod install", "-workspace Sample.xcworkspace", "-scheme Sample -configuration Debug"}
	last := -1
	for _, want := range order {
		i := strings.Index(got, want)
		if i < 0 || i < last {
			t.Fatalf("expected %q in order; calls:\n%s", want, got)
		}
		last = i
	}
}

func TestNpmInstallRetriesWithLegacyPeerDeps(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "package.json", sparklingPackage)
	writeFile(t, root, ".git/config", "[core]\n")
	marker := t.TempDir() + "/failed-once"
	// the strict install fails once (as the npm resolver does on some fresh scaffolds), the retry succeeds
	calls := installFakeTools(t, map[string]string{
		"npm": "case \"$*\" in 'install') [ -e '" + marker + "' ] || { touch '" + marker + "'; exit 1; };; esac",
	})
	_ = BuildUnsigned(t.Context(), sparklingOptions(root, t.TempDir()))
	got := strings.Join(readCalls(t, calls), "\n")
	if !strings.Contains(got, "npm install @") || !strings.Contains(got, "npm install --legacy-peer-deps") || !strings.Contains(got, "npm run build") {
		t.Fatalf("install was not retried before the build:\n%s", got)
	}
}
