package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tauriFixture(t *testing.T, withGen bool) string {
	root := t.TempDir()
	writeFile(t, root, "package.json", `{"devDependencies":{"@tauri-apps/cli":"^2.0.0"}}`)
	writeFile(t, root, "src-tauri/tauri.conf.json", `{"identifier":"example.generic.tauri"}`)
	if withGen {
		writeFile(t, root, "src-tauri/gen/apple/app.xcodeproj/project.pbxproj", "{}")
	}
	return root
}

func fakeTauriTools(t *testing.T, root string) string {
	ipa := filepath.Join(t.TempDir(), "sample.ipa")
	writeSampleIPA(t, ipa, "example.generic.tauri")
	built := filepath.Join(root, "src-tauri", "gen", "apple", "build", "arm64", "Sample.ipa")
	return installFakeTools(t, map[string]string{
		"npm":    "",
		"rustup": "",
		// the fake CLI "builds" by dropping the IPA where Tauri puts it
		"npx": "case \"$*\" in *'ios build'*) mkdir -p '" + filepath.Dir(built) + "' && cp '" + ipa + "' '" + built + "';; esac",
	})
}

func TestTauriRunsInitThenUnsignedBuild(t *testing.T) {
	root := tauriFixture(t, false)
	calls := fakeTauriTools(t, root)
	info := buildFixture(t, root, &BuildOptions{Framework: FrameworkTauri, IOSPath: "src-tauri/gen/apple", BundleID: "example.generic.tauri"})
	if info.BundleID != "example.generic.tauri" {
		t.Fatalf("unexpected application: %+v", info)
	}
	got := strings.Join(readCalls(t, calls), "\n")
	order := []string{"npm install", "rustup target add aarch64-apple-ios", "npx --no-install tauri ios init --ci", "npx --no-install tauri ios build --ci --no-sign --debug"}
	last := -1
	for _, want := range order {
		i := strings.Index(got, want)
		if i < 0 || i < last {
			t.Fatalf("expected %q in order; calls:\n%s", want, got)
		}
		last = i
	}
}

func TestTauriSkipsInitWhenProjectIsCommitted(t *testing.T) {
	root := tauriFixture(t, true)
	calls := fakeTauriTools(t, root)
	buildFixture(t, root, &BuildOptions{Framework: FrameworkTauri, IOSPath: "src-tauri/gen/apple", Configuration: "Release"})
	got := strings.Join(readCalls(t, calls), "\n")
	if strings.Contains(got, "ios init") {
		t.Fatalf("init ran although gen/apple is committed:\n%s", got)
	}
	if !strings.Contains(got, "ios build --ci --no-sign") || strings.Contains(got, "--debug") {
		t.Fatalf("release build arguments wrong:\n%s", got)
	}
}

func TestTauriFailsWithoutIPAOrWithWrongIdentity(t *testing.T) {
	root := tauriFixture(t, true)
	installFakeTools(t, map[string]string{"npm": "", "rustup": "", "npx": ""})
	output := filepath.Join(t.TempDir(), "private-output")
	options := &BuildOptions{
		Framework: FrameworkTauri, IOSPath: "src-tauri/gen/apple", Configuration: "Debug", SourceRoot: root,
		LogPath: filepath.Join(output, "build.log"), IPAPath: filepath.Join(output, "App.ipa"),
	}
	writeFile(t, root, ".git/config", "[core]\n")
	if err := BuildUnsigned(t.Context(), options); err == nil {
		t.Fatal("build without an IPA succeeded")
	}
	log, _ := os.ReadFile(options.LogPath)
	if !strings.Contains(string(log), "left no .ipa") {
		t.Fatalf("log does not explain the missing IPA:\n%s", log)
	}

	root = tauriFixture(t, true)
	fakeTauriTools(t, root)
	options.SourceRoot, options.BundleID = root, "example.generic.other"
	writeFile(t, root, ".git/config", "[core]\n")
	if err := BuildUnsigned(t.Context(), options); err == nil {
		t.Fatal("IPA with a different identity accepted")
	}
}
