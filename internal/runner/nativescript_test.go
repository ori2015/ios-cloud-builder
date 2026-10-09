package runner

import (
	"strings"
	"testing"
)

func TestNativeScriptPreparesThenBuildsWithXcode(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "package.json", `{"dependencies":{"@nativescript/core":"~8.8.0"},"devDependencies":{"nativescript":"^8.8.0"}}`)
	writeFile(t, root, "nativescript.config.ts", "export default { id: 'example.generic.ns', appPath: 'src' }")
	calls := installFakeTools(t, map[string]string{
		"npm": "",
		// `ns prepare ios` creates the Xcode project the shared build then compiles
		"npx":        "case \"$*\" in *'prepare ios'*) mkdir -p platforms/ios/Sample.xcodeproj;; esac",
		"xcodebuild": "case \"$*\" in *-list*) echo '{\"project\":{\"schemes\":[\"Sample\",\"SampleTests\"]}}';; esac",
	})
	output := t.TempDir()
	options := &BuildOptions{
		Framework: FrameworkNativeScript, IOSPath: "platforms/ios", Configuration: "Release", SourceRoot: root,
		LogPath: output + "/private-output/build.log", IPAPath: output + "/private-output/App.ipa",
	}
	writeFile(t, root, ".git/config", "[core]\n")
	_ = BuildUnsigned(t.Context(), options) // packaging needs macOS ditto; the sequence before it is what is under test
	got := strings.Join(readCalls(t, calls), "\n")
	order := []string{
		"npm install",
		"npx --no-install nativescript prepare ios --release",
		"xcodebuild -project Sample.xcodeproj -list -json",
		"-scheme Sample -configuration Release -destination generic/platform=iOS",
	}
	last := -1
	for _, want := range order {
		i := strings.Index(got, want)
		if i < 0 || i < last {
			t.Fatalf("expected %q in order; calls:\n%s", want, got)
		}
		last = i
	}
	if strings.Contains(got, "-scheme SampleTests") {
		t.Fatal("the test scheme was chosen")
	}
}

func TestNativeScriptFetchesCLIWhenNotADependency(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "package.json", `{"dependencies":{"@nativescript/core":"~8.8.0"}}`)
	writeFile(t, root, "nativescript.config.ts", "export default { id: 'example.generic.ns' }")
	calls := installFakeTools(t, map[string]string{"npm": "", "npx": "", "xcodebuild": ""})
	output := t.TempDir()
	options := &BuildOptions{
		Framework: FrameworkNativeScript, IOSPath: "platforms/ios", Configuration: "Debug", SourceRoot: root,
		LogPath: output + "/private-output/build.log", IPAPath: output + "/private-output/App.ipa",
	}
	writeFile(t, root, ".git/config", "[core]\n")
	_ = BuildUnsigned(t.Context(), options)
	if got := strings.Join(readCalls(t, calls), "\n"); !strings.Contains(got, "npx --yes nativescript prepare ios") || strings.Contains(got, "--release") {
		t.Fatalf("unexpected prepare invocation:\n%s", got)
	}
}
