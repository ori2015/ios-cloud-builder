package runner

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"filippo.io/age"
	"howett.net/plist"
)

const testXcentWithDomains = `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>application-identifier</key><string>TEAM123456.com.example.app</string>
<key>com.apple.developer.associated-domains</key>
<array><string>applinks:app.example.com</string><string>applinks:www.example.com</string></array>
</dict></plist>`

func TestSanitizeAssociatedDomains(t *testing.T) {
	valid := []string{"applinks:example.com", "applinks:app.example.com", "applinks:*.example.com", "applinks:a-b.example.co.uk"}
	kept, dropped := sanitizeAssociatedDomains(valid)
	if dropped != 0 || len(kept) != len(valid) {
		t.Fatalf("valid entries rejected: kept=%v dropped=%d", kept, dropped)
	}
	for name, value := range map[string]string{
		"developer mode":      "applinks:example.com?mode=developer",
		"other service":       "webcredentials:example.com",
		"wildcard only":       "*",
		"empty host":          "applinks:",
		"single label":        "applinks:localhost",
		"path":                "applinks:example.com/path",
		"port":                "applinks:example.com:8080",
		"space":               "applinks:exa mple.com",
		"newline":             "applinks:example.com\n",
		"leading hyphen":      "applinks:-example.com",
		"trailing hyphen":     "applinks:example-.com",
		"double dot":          "applinks:example..com",
		"scheme confusion":    "applinks:https://example.com",
		"control character":   "applinks:exam\x00ple.com",
		"too long":            "applinks:" + strings.Repeat("a", 300) + ".com",
		"inner wildcard":      "applinks:a.*.example.com",
		"missing prefix":      "example.com",
		"uppercase prefix":    "APPLINKS:example.com",
		"unicode lookalike":   "applinks:exаmple.com",
		"credentials in host": "applinks:user@example.com",
	} {
		if kept, _ := sanitizeAssociatedDomains([]string{value}); len(kept) != 0 {
			t.Errorf("%s accepted: %q", name, value)
		}
	}
	kept, dropped = sanitizeAssociatedDomains([]string{"applinks:b.example.com", "applinks:a.example.com", "applinks:a.example.com"})
	if dropped != 1 || !reflect.DeepEqual(kept, []string{"applinks:a.example.com", "applinks:b.example.com"}) {
		t.Fatalf("de-duplication/order wrong: kept=%v dropped=%d", kept, dropped)
	}
	many := make([]string, 0, maxAssociatedDomains+5)
	for i := 0; i < maxAssociatedDomains+5; i++ {
		many = append(many, "applinks:h"+strings.Repeat("a", i)+".example.com")
	}
	if kept, _ := sanitizeAssociatedDomains(many); len(kept) != maxAssociatedDomains {
		t.Fatalf("cap not applied: %d entries", len(kept))
	}
}

func TestValidateAssociatedDomainsFailsClosed(t *testing.T) {
	if err := validateAssociatedDomains(nil); err != nil {
		t.Fatalf("empty list rejected: %v", err)
	}
	if err := validateAssociatedDomains([]string{"applinks:a.example.com", "applinks:b.example.com"}); err != nil {
		t.Fatalf("canonical list rejected: %v", err)
	}
	for name, values := range map[string][]string{
		"invalid":     {"applinks:example.com?mode=developer"},
		"duplicate":   {"applinks:a.example.com", "applinks:a.example.com"},
		"unsorted":    {"applinks:b.example.com", "applinks:a.example.com"},
		"wrong scope": {"webcredentials:example.com"},
	} {
		if err := validateAssociatedDomains(values); err == nil {
			t.Errorf("%s list accepted", name)
		}
	}
}

// Regression: the profile's wildcard must not reach codesign when the project
// declared a concrete applinks host.
func TestWildcardProfileWithApplinksRequestSignsApplinksDomain(t *testing.T) {
	profile := map[string]any{
		"application-identifier":              "TEAM123456.com.example.app",
		"get-task-allow":                      false,
		"keychain-access-groups":              []any{"TEAM123456.*"},
		associatedDomainsKey:                  "*",
		"com.apple.developer.team-identifier": "TEAM123456",
	}
	path := filepath.Join(t.TempDir(), "entitlements.plist")
	if err := writeSigningEntitlements(path, profile, []string{"applinks:app.example.com"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var signed map[string]any
	if _, err := plist.Unmarshal(data, &signed); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(signed[associatedDomainsKey], []any{"applinks:app.example.com"}) {
		t.Fatalf("signed associated-domains = %#v, want [applinks:app.example.com]", signed[associatedDomainsKey])
	}
	for key, want := range profile {
		if key != associatedDomainsKey && !reflect.DeepEqual(signed[key], want) {
			t.Errorf("%s changed: %#v", key, signed[key])
		}
	}
	if profile[associatedDomainsKey] != "*" {
		t.Fatal("the profile's entitlements were modified in place")
	}
}

func TestMergeAssociatedDomainsNeverWidensTheProfile(t *testing.T) {
	base := func(value any) map[string]any {
		return map[string]any{"application-identifier": "TEAM123456.com.example.app", associatedDomainsKey: value}
	}
	request := []string{"applinks:a.example.com", "applinks:b.example.com"}
	t.Run("wildcard array", func(t *testing.T) {
		got := mergeAssociatedDomains(base([]any{"*"}), request)
		if !reflect.DeepEqual(got[associatedDomainsKey], request) {
			t.Fatalf("got %#v", got[associatedDomainsKey])
		}
	})
	t.Run("explicit list grants only the intersection", func(t *testing.T) {
		got := mergeAssociatedDomains(base([]any{"applinks:a.example.com", "applinks:other.example.com"}), request)
		if !reflect.DeepEqual(got[associatedDomainsKey], []string{"applinks:a.example.com"}) {
			t.Fatalf("got %#v", got[associatedDomainsKey])
		}
	})
	t.Run("nothing permitted leaves the profile unchanged", func(t *testing.T) {
		profile := base([]any{"applinks:other.example.com"})
		got := mergeAssociatedDomains(profile, request)
		if !reflect.DeepEqual(got, profile) {
			t.Fatalf("got %#v", got)
		}
	})
	t.Run("profile without the key never gains it", func(t *testing.T) {
		profile := map[string]any{"application-identifier": "TEAM123456.com.example.app"}
		got := mergeAssociatedDomains(profile, request)
		if _, ok := got[associatedDomainsKey]; ok {
			t.Fatal("associated-domains added to a profile that does not grant it")
		}
	})
	t.Run("no request keeps the profile value", func(t *testing.T) {
		got := mergeAssociatedDomains(base("*"), nil)
		if got[associatedDomainsKey] != "*" {
			t.Fatalf("got %#v", got[associatedDomainsKey])
		}
	})
	t.Run("unexpected value type is left alone", func(t *testing.T) {
		got := mergeAssociatedDomains(base(true), request)
		if got[associatedDomainsKey] != true {
			t.Fatalf("got %#v", got[associatedDomainsKey])
		}
	})
}

func TestCopyEntitlementsRequestFindsTheBuildEntitlements(t *testing.T) {
	derived := t.TempDir()
	intermediates := filepath.Join(derived, "Build", "Intermediates.noindex", "Proj.build", "Release-iphoneos", "App.build")
	if err := os.MkdirAll(intermediates, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(intermediates, "App.app.xcent"), []byte(testXcentWithDomains), 0600); err != nil {
		t.Fatal(err)
	}
	// A different configuration and a different app must not be picked up.
	other := filepath.Join(derived, "Build", "Intermediates.noindex", "Proj.build", "Debug-iphoneos", "App.build")
	if err := os.MkdirAll(other, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "App.app.xcent"), []byte("wrong configuration"), 0600); err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(t.TempDir(), "App.app")
	if err := os.MkdirAll(app, 0700); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	copyEntitlementsRequest(derived, app, "Release", &log)
	data, err := os.ReadFile(filepath.Join(app, entitlementsRequestFile))
	if err != nil || string(data) != testXcentWithDomains {
		t.Fatalf("entitlements request not recorded: err=%v data=%q", err, data)
	}
}

func TestCopyEntitlementsRequestIsBestEffort(t *testing.T) {
	app := filepath.Join(t.TempDir(), "App.app")
	if err := os.MkdirAll(app, 0700); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	copyEntitlementsRequest(t.TempDir(), app, "Release", &log)
	if _, err := os.Stat(filepath.Join(app, entitlementsRequestFile)); !os.IsNotExist(err) {
		t.Fatalf("a request file appeared without a build entitlements file: %v", err)
	}
	if !strings.Contains(log.String(), "as-is") {
		t.Fatalf("missing explanation in the private log: %q", log.String())
	}
}

func TestTakeAssociatedDomainsRequest(t *testing.T) {
	newApp := func(t *testing.T) string {
		app := filepath.Join(t.TempDir(), "App.app")
		if err := os.MkdirAll(app, 0700); err != nil {
			t.Fatal(err)
		}
		return app
	}
	t.Run("valid", func(t *testing.T) {
		app := newApp(t)
		if err := os.WriteFile(filepath.Join(app, entitlementsRequestFile), []byte(testXcentWithDomains), 0600); err != nil {
			t.Fatal(err)
		}
		var log bytes.Buffer
		got := takeAssociatedDomainsRequest(app, &log)
		if !reflect.DeepEqual(got, []string{"applinks:app.example.com", "applinks:www.example.com"}) {
			t.Fatalf("domains = %v", got)
		}
		if _, err := os.Stat(filepath.Join(app, entitlementsRequestFile)); !os.IsNotExist(err) {
			t.Fatal("the request file was left in the bundle to be signed")
		}
		if strings.Contains(log.String(), "example.com") {
			t.Fatalf("domain values were written to the log: %q", log.String())
		}
	})
	t.Run("hostile and unusable input is ignored but still removed", func(t *testing.T) {
		for name, content := range map[string][]byte{
			"not a plist": []byte("not a plist"),
			"oversize":    bytes.Repeat([]byte("x"), maxEntitlementsRequestBytes+1),
			"wrong shape": []byte(`<?xml version="1.0"?><plist version="1.0"><dict><key>com.apple.developer.associated-domains</key><string>*</string></dict></plist>`),
			"bad entries": []byte(`<?xml version="1.0"?><plist version="1.0"><dict><key>com.apple.developer.associated-domains</key><array><string>applinks:evil.com?mode=developer</string><string>keychain</string></array></dict></plist>`),
		} {
			app := newApp(t)
			if err := os.WriteFile(filepath.Join(app, entitlementsRequestFile), content, 0600); err != nil {
				t.Fatal(err)
			}
			if got := takeAssociatedDomainsRequest(app, &bytes.Buffer{}); len(got) != 0 {
				t.Errorf("%s produced domains: %v", name, got)
			}
			if _, err := os.Lstat(filepath.Join(app, entitlementsRequestFile)); !os.IsNotExist(err) {
				t.Errorf("%s: request file remained", name)
			}
		}
	})
	t.Run("symlink is not followed", func(t *testing.T) {
		app := newApp(t)
		target := filepath.Join(t.TempDir(), "outside.xcent")
		if err := os.WriteFile(target, []byte(testXcentWithDomains), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(app, entitlementsRequestFile)); err != nil {
			t.Skip("symlinks unavailable")
		}
		if got := takeAssociatedDomainsRequest(app, &bytes.Buffer{}); len(got) != 0 {
			t.Fatalf("symlinked request produced domains: %v", got)
		}
		if _, err := os.Stat(target); err != nil {
			t.Fatal("the symlink target was removed")
		}
	})
	t.Run("absent", func(t *testing.T) {
		if got := takeAssociatedDomainsRequest(newApp(t), &bytes.Buffer{}); got != nil {
			t.Fatalf("domains = %v", got)
		}
	})
}

func TestProvenanceRejectsInvalidAssociatedDomains(t *testing.T) {
	for name, domains := range map[string][]string{
		"developer mode": {"applinks:example.com?mode=developer"},
		"unsorted":       {"applinks:b.example.com", "applinks:a.example.com"},
		"other service":  {"webcredentials:example.com"},
	} {
		t.Run(name, func(t *testing.T) {
			dir, expected := provenanceFixture(t)
			manifest, err := ValidateProvenanceArtifact(dir, expected)
			if err != nil {
				t.Fatal(err)
			}
			manifest.AssociatedDomains = domains
			writeManifest(t, dir, manifest)
			if _, err := ValidateProvenanceArtifact(dir, expected); err == nil {
				t.Fatal("manifest with invalid associated domains accepted")
			}
		})
	}
	dir, expected := provenanceFixture(t)
	manifest, err := ValidateProvenanceArtifact(dir, expected)
	if err != nil {
		t.Fatal(err)
	}
	manifest.AssociatedDomains = []string{"applinks:a.example.com", "applinks:b.example.com"}
	writeManifest(t, dir, manifest)
	got, err := ValidateProvenanceArtifact(dir, expected)
	if err != nil || !reflect.DeepEqual(got.AssociatedDomains, manifest.AssociatedDomains) {
		t.Fatalf("valid associated domains rejected or altered: %v %v", got, err)
	}
}

// End to end through the trusted packager: a project-declared applinks host
// reaches the authenticated manifest, and the request file is never packaged.
func TestTrustedPackageCarriesAssociatedDomains(t *testing.T) {
	packagingIdentity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	signingIdentity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	infoPlist := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>com.example.private</string>
<key>CFBundleShortVersionString</key><string>1.2.3</string>
<key>CFBundleExecutable</key><string>App</string>
</dict></plist>`
	inputIPA := writeDeployTestZIP(t, map[string]zipEntry{
		"Payload/":                                   {mode: os.ModeDir | 0755},
		"Payload/App.app/":                           {mode: os.ModeDir | 0755},
		"Payload/App.app/Info.plist":                 {data: infoPlist, mode: 0644},
		"Payload/App.app/App":                        {data: "Mach-O fixture", mode: 0755},
		"Payload/App.app/" + entitlementsRequestFile: {data: testXcentWithDomains, mode: 0644},
	})
	inputDir := filepath.Join(t.TempDir(), "input")
	if err := os.Mkdir(inputDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := encryptAndRemove(packagingIdentity.Recipient(), inputIPA, filepath.Join(inputDir, projectOutputFile), true); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inputDir, "build.log.age"), []byte("encrypted diagnostic fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	outputDir := filepath.Join(t.TempDir(), "trusted-output")
	options := &TrustedPackageOptions{
		InputDir: inputDir, OutputDir: outputDir,
		BuildID: "123e4567-e89b-42d3-a456-426614174000", ProjectID: "p_0123456789abcdef0123456789abcdef",
		BuilderCommit: strings.Repeat("a", 40), WorkflowRef: "owner/repo/.github/workflows/ios-build.yml@refs/heads/main",
	}
	t.Setenv("PACKAGING_AGE_IDENTITY", packagingIdentity.String())
	packager := func(run executor, appPath, outputPath string) error {
		if _, err := os.Lstat(filepath.Join(appPath, entitlementsRequestFile)); !os.IsNotExist(err) {
			t.Error("the request file was still in the application when it was packaged for signing")
		}
		return packageAppFixture(run, appPath, outputPath)
	}
	if err := trustedPackageWithPackager(t.Context(), options, signingIdentity.Recipient().String(), os.Stderr, packager); err != nil {
		t.Fatal(err)
	}
	expected := ProvenanceExpectation{
		BuildID: options.BuildID, ProjectID: options.ProjectID,
		BuilderCommit: options.BuilderCommit, WorkflowRef: options.WorkflowRef,
	}
	manifest, err := ValidateProvenanceArtifact(outputDir, expected)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"applinks:app.example.com", "applinks:www.example.com"}; !reflect.DeepEqual(manifest.AssociatedDomains, want) {
		t.Fatalf("manifest associated domains = %v, want %v", manifest.AssociatedDomains, want)
	}
}
