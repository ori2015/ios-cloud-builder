package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const mauiCsproj = `<Project Sdk="Microsoft.NET.Sdk"><PropertyGroup>
<TargetFrameworks>net8.0-android;net8.0-ios;net8.0-maccatalyst</TargetFrameworks>
<UseMaui>true</UseMaui><ApplicationId>example.generic.maui</ApplicationId></PropertyGroup></Project>`

func mauiBuildOptions(root string) *BuildOptions {
	return &BuildOptions{Framework: FrameworkMAUI, IOSPath: ".", BundleID: "example.generic.maui", Configuration: "Debug", SourceRoot: root}
}

func TestMAUIRestoresWorkloadsThenPublishesAnUnsignedIPA(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "Generic.csproj", mauiCsproj)
	ipa := filepath.Join(t.TempDir(), "sample.ipa")
	writeSampleIPA(t, ipa, "example.generic.maui")
	published := filepath.Join(root, "bin", "Release", "net8.0-ios", "ios-arm64", "publish", "Generic.ipa")
	calls := installFakeTools(t, map[string]string{
		"dotnet": "case \"$1\" in publish) mkdir -p '" + filepath.Dir(published) + "' && cp '" + ipa + "' '" + published + "';; esac",
	})
	info := buildFixture(t, root, mauiBuildOptions(root))
	if info.BundleID != "example.generic.maui" {
		t.Fatalf("unexpected application: %+v", info)
	}
	got := strings.Join(readCalls(t, calls), "\n")
	restore := strings.Index(got, "dotnet workload restore Generic.csproj")
	publish := strings.Index(got, "dotnet publish Generic.csproj -f net8.0-ios -c Release -r ios-arm64 -p:EnableCodeSigning=false -p:BuildIpa=true")
	if restore < 0 || publish < restore {
		t.Fatalf("commands wrong or out of order:\n%s", got)
	}
}

func TestMAUIExplainsXcodeVersionMismatch(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "Generic.csproj", mauiCsproj)
	writeFile(t, root, ".git/config", "[core]\n")
	installFakeTools(t, map[string]string{
		"dotnet": "case \"$1\" in publish) echo 'error : This version of .NET for iOS (26.5.9004) requires Xcode 26.5. The current version of Xcode is 16.4. Either install Xcode 26.5'; exit 1;; esac",
	})
	output := t.TempDir() + "/private-output"
	options := mauiBuildOptions(root)
	options.LogPath, options.IPAPath = output+"/build.log", output+"/App.ipa"
	if err := BuildUnsigned(t.Context(), options); err == nil {
		t.Fatal("failed publish reported success")
	}
	log, _ := os.ReadFile(options.LogPath)
	for _, want := range []string{"needs Xcode 26.5", "uses Xcode 16.4", ".xcode-version", "workloadVersion"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
}

func TestMAUIWithoutIPAOrProjectFailsClearly(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "Generic.csproj", mauiCsproj)
	writeFile(t, root, ".git/config", "[core]\n")
	installFakeTools(t, map[string]string{"dotnet": ""})
	output := t.TempDir() + "/private-output"
	options := mauiBuildOptions(root)
	options.LogPath, options.IPAPath = output+"/build.log", output+"/App.ipa"
	if err := BuildUnsigned(t.Context(), options); err == nil {
		t.Fatal("build without an IPA succeeded")
	}
	log, _ := os.ReadFile(options.LogPath)
	if !strings.Contains(string(log), "left no .ipa") {
		t.Fatalf("log does not explain the missing IPA:\n%s", log)
	}
}
