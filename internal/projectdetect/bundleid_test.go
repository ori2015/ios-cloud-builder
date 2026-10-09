package projectdetect

import (
	"strings"
	"testing"
)

func pbxproj(settings ...[2]string) string {
	var b strings.Builder
	b.WriteString("// !$*UTF8*$!\n{\n\tobjects = {\n")
	for i, s := range settings {
		b.WriteString("\t\tAA" + string(rune('A'+i)) + " /* " + s[0] + " */ = {\n\t\t\tisa = XCBuildConfiguration;\n\t\t\tbuildSettings = {\n")
		b.WriteString("\t\t\t\tPRODUCT_BUNDLE_IDENTIFIER = " + s[1] + ";\n")
		b.WriteString("\t\t\t};\n\t\t\tname = " + s[0] + ";\n\t\t};\n")
	}
	b.WriteString("\t};\n}\n")
	return b.String()
}

func TestBundleID(t *testing.T) {
	cases := []struct {
		name       string
		setup      func(t *testing.T, r string)
		layoutKind Kind
		appPath    string
		iosPath    string
		config     string
		want       string
		wantReason string
	}{
		{"flutter runner", func(t *testing.T, r string) {
			write(t, r, "ios/Runner.xcodeproj/project.pbxproj", pbxproj(
				[2]string{"Debug", "example.generic.app"}, [2]string{"Release", "example.generic.app"},
				[2]string{"Debug", "example.generic.app.RunnerTests"}))
		}, KindFlutter, ".", "ios", "Debug", "example.generic.app", ""},
		{"debug-only suffix is respected per configuration", func(t *testing.T, r string) {
			write(t, r, "App.xcodeproj/project.pbxproj", pbxproj(
				[2]string{"Debug", "example.generic.app.dev"}, [2]string{"Release", "example.generic.app"}))
		}, KindXcode, ".", ".", "Debug", "example.generic.app.dev", ""},
		{"quoted identifier", func(t *testing.T, r string) {
			write(t, r, "App.xcodeproj/project.pbxproj", pbxproj([2]string{"Debug", `"example.generic.q"`}))
		}, KindXcode, ".", ".", "Debug", "example.generic.q", ""},
		{"build variable is not guessed", func(t *testing.T, r string) {
			write(t, r, "ios/App.xcodeproj/project.pbxproj", pbxproj([2]string{"Debug", `"org.example.$(PRODUCT_NAME:rfc1034identifier)"`}))
		}, KindNode, ".", "ios", "Debug", "", "build variables"},
		{"unrelated targets are not guessed", func(t *testing.T, r string) {
			write(t, r, "App.xcodeproj/project.pbxproj", pbxproj(
				[2]string{"Debug", "example.one.app"}, [2]string{"Debug", "example.two.app"}))
		}, KindXcode, ".", ".", "Debug", "", "unrelated"},
		{"pods project ignored", func(t *testing.T, r string) {
			write(t, r, "ios/App.xcodeproj/project.pbxproj", pbxproj([2]string{"Debug", "example.generic.app"}))
			write(t, r, "ios/Pods.xcodeproj/project.pbxproj", pbxproj([2]string{"Debug", "org.cocoapods.Thing"}))
		}, KindNode, ".", "ios", "Debug", "example.generic.app", ""},
		{"xcodegen manifest", func(t *testing.T, r string) {
			write(t, r, "project.yml", "name: A\ntargets:\n  A:\n    settings:\n      base:\n        PRODUCT_BUNDLE_IDENTIFIER: example.generic.gen\n")
		}, KindXcodeGen, ".", ".", "Debug", "example.generic.gen", ""},
		{"managed expo app.json", func(t *testing.T, r string) {
			write(t, r, "package.json", expoPackage)
			write(t, r, "app.json", `{"expo":{"ios":{"bundleIdentifier":"example.generic.expo"}}}`)
		}, KindNode, ".", "ios", "Debug", "example.generic.expo", ""},
		{"capacitor ts config before cap add", func(t *testing.T, r string) {
			write(t, r, "package.json", capPackage)
			write(t, r, "capacitor.config.ts", "const config = {\n  appId: 'example.generic.cap',\n  webDir: 'dist',\n};\nexport default config;\n")
		}, KindNode, ".", "ios", "Debug", "example.generic.cap", ""},
		{"nothing to read", func(t *testing.T, r string) { write(t, r, "package.json", expoPackage) }, KindNode, ".", "ios", "Debug", "", "no literal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tc.setup(t, root)
			got, reason := BundleID(root, &Layout{AppPath: tc.appPath, IOSPath: tc.iosPath, Kind: tc.layoutKind}, tc.config)
			if got != tc.want || (tc.wantReason == "") != (reason == "") || !strings.Contains(reason, tc.wantReason) {
				t.Fatalf("got %q (%q); want %q (reason containing %q)", got, reason, tc.want, tc.wantReason)
			}
		})
	}
}
