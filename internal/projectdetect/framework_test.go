package projectdetect

import (
	"errors"
	"strings"
	"testing"
)

func TestDetectFramework(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, r string)
		want  string
	}{
		{"flutter", func(t *testing.T, r string) { write(t, r, "pubspec.yaml", flutterPubspec) }, FrameworkFlutter},
		{"managed expo has no ios folder", func(t *testing.T, r string) { write(t, r, "package.json", expoPackage) }, FrameworkExpo},
		{"expo with committed ios builds as react native", func(t *testing.T, r string) {
			write(t, r, "package.json", expoPackage)
			mkdir(t, r, "ios/Generic.xcodeproj")
		}, FrameworkReactNative},
		{"react native", func(t *testing.T, r string) { write(t, r, "package.json", rnPackage) }, FrameworkReactNative},
		{"capacitor with config", func(t *testing.T, r string) {
			write(t, r, "package.json", capPackage)
			write(t, r, "capacitor.config.json", `{"appId":"example.generic.cap","webDir":"dist"}`)
		}, FrameworkIonic},
		{"ionic", func(t *testing.T, r string) { write(t, r, "package.json", `{"dependencies":{"@ionic/angular":"7"}}`) }, FrameworkIonic},
		{"cordova by dependency", func(t *testing.T, r string) { write(t, r, "package.json", `{"dependencies":{"cordova":"12"}}`) }, FrameworkCordova},
		{"cordova by config.xml", func(t *testing.T, r string) { write(t, r, "config.xml", cordovaConfig) }, FrameworkCordova},
		{"kmp in a module build file", func(t *testing.T, r string) {
			write(t, r, "shared/build.gradle.kts", `plugins { kotlin("multiplatform") }`)
		}, FrameworkKMP},
		{"kmp in a version catalog", func(t *testing.T, r string) {
			write(t, r, "gradle/libs.versions.toml", `kotlinMultiplatform = { id = "org.jetbrains.kotlin.multiplatform", version = "2.0.0" }`)
		}, FrameworkKMP},
		{"native xcodeproj", func(t *testing.T, r string) { mkdir(t, r, "Generic.xcodeproj") }, FrameworkNative},
		{"native in ios subfolder", func(t *testing.T, r string) { mkdir(t, r, "ios/Generic.xcodeproj") }, FrameworkNative},
		{"xcodegen", func(t *testing.T, r string) { write(t, r, "project.yml", xcodegenYML) }, FrameworkNative},
		{"dart-only pubspec with a native project", func(t *testing.T, r string) {
			write(t, r, "pubspec.yaml", "name: tool\n")
			mkdir(t, r, "Generic.xcodeproj")
		}, FrameworkNative},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tc.setup(t, root)
			got, err := DetectFramework(root)
			if err != nil || got != tc.want {
				t.Fatalf("DetectFramework = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestDetectFrameworkErrors(t *testing.T) {
	empty := t.TempDir()
	if _, err := DetectFramework(empty); !errors.Is(err, ErrUnrecognized) {
		t.Fatalf("empty directory: %v", err)
	}
	spm := t.TempDir()
	write(t, spm, "Package.swift", "// swift-tools-version:5.9\n")
	_, err := DetectFramework(spm)
	if !errors.Is(err, ErrUnrecognized) || err == nil {
		t.Fatalf("swift package without an app: %v", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "Package.swift") || !strings.Contains(msg, "app project") {
		t.Fatalf("message does not say what is missing and what to add: %s", msg)
	}
	tooling := t.TempDir()
	write(t, tooling, "package.json", `{"devDependencies":{"prettier":"3"}}`)
	if _, err := DetectFramework(tooling); !errors.Is(err, ErrUnrecognized) {
		t.Fatalf("tooling-only package.json recognised as an app: %v", err)
	}
}

func TestIsFlutterPubspec(t *testing.T) {
	for content, want := range map[string]bool{
		"flutter:\n  uses-material-design: true\n":      true,
		"dependencies:\n  flutter:\n    sdk: flutter\n": true,
		"name: dart_tool\ndependencies:\n  path: ^1\n":  false,
		"description: uses flutter: somewhere":          false,
	} {
		if got := IsFlutterPubspec([]byte(content)); got != want {
			t.Errorf("IsFlutterPubspec(%q) = %v, want %v", content, got, want)
		}
	}
}
