package runner

import (
	"archive/zip"
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"howett.net/plist"
)

func TestProfileMatchesBundle(t *testing.T) {
	for _, test := range []struct {
		applicationID string
		bundleID      string
		want          bool
	}{
		{"TEAM123456.com.example.app", "com.example.app", true},
		{"TEAM123456.com.example.*", "com.example.app", false},
		{"OTHER12345.com.example.app", "com.example.app", false},
		{"TEAM123456.com.other.app", "com.example.app", false},
	} {
		if got := profileMatchesBundle(test.applicationID, "TEAM123456", test.bundleID); got != test.want {
			t.Fatalf("profileMatchesBundle(%q, %q) = %v", test.applicationID, test.bundleID, got)
		}
	}
}

func TestValidateAppStoreProfile(t *testing.T) {
	valid := provisioningProfile{
		ExpirationDate:        time.Now().Add(time.Hour),
		Platform:              []string{"iOS"},
		DeveloperCertificates: [][]byte{[]byte("certificate")},
		Entitlements:          map[string]any{"get-task-allow": false},
	}
	if err := validateAppStoreProfile(&valid); err != nil {
		t.Fatalf("valid profile rejected: %v", err)
	}
	for name, mutate := range map[string]func(*provisioningProfile){
		"expired":      func(profile *provisioningProfile) { profile.ExpirationDate = time.Now().Add(-time.Minute) },
		"wrong target": func(profile *provisioningProfile) { profile.Platform = []string{"tvOS"} },
		"devices":      func(profile *provisioningProfile) { profile.ProvisionedDevices = []string{"device"} },
		"enterprise":   func(profile *provisioningProfile) { profile.ProvisionsAllDevices = true },
		"debuggable":   func(profile *provisioningProfile) { profile.Entitlements["get-task-allow"] = true },
	} {
		t.Run(name, func(t *testing.T) {
			profile := valid
			profile.Platform = append([]string(nil), valid.Platform...)
			profile.DeveloperCertificates = append([][]byte(nil), valid.DeveloperCertificates...)
			profile.Entitlements = map[string]any{"get-task-allow": false}
			mutate(&profile)
			if err := validateAppStoreProfile(&profile); err == nil {
				t.Fatal("unsafe profile was accepted")
			}
		})
	}
}

func TestProfileAuthorizesIdentity(t *testing.T) {
	certificate := []byte("certificate DER")
	fingerprint := sha1.Sum(certificate) // #nosec G401 -- mirrors Apple's certificate fingerprint.
	identity := strings.ToUpper(hex.EncodeToString(fingerprint[:]))
	if !profileAuthorizesIdentity([][]byte{certificate}, identity) {
		t.Fatal("profile certificate was not matched")
	}
	if profileAuthorizesIdentity([][]byte{[]byte("other")}, identity) {
		t.Fatal("unrelated certificate was matched")
	}
}

func TestSigningIdentityPatternCapturesFingerprintAndName(t *testing.T) {
	output := `  1) 3A1C0EDBC4757C7C1CECE2469D5A878EEA8C2292 "Apple Distribution: Example (TEAM123456)"`
	match := identityPattern.FindStringSubmatch(output)
	if len(match) != 3 || match[1] != "3A1C0EDBC4757C7C1CECE2469D5A878EEA8C2292" ||
		match[2] != "Apple Distribution: Example (TEAM123456)" {
		t.Fatalf("identity match = %#v", match)
	}
}

func TestExtractUnsignedIPARejectsTraversalAndSymlink(t *testing.T) {
	for name, entries := range map[string]map[string]zipEntry{
		"traversal": {"Payload/App.app/../../secret": {data: "x", mode: 0600}},
		"symlink":   {"Payload/App.app/link": {data: "target", mode: os.ModeSymlink | 0777}},
	} {
		t.Run(name, func(t *testing.T) {
			ipa := writeDeployTestZIP(t, entries)
			if _, err := extractUnsignedIPA(ipa, filepath.Join(t.TempDir(), "out")); err == nil {
				t.Fatal("unsafe IPA was accepted")
			}
		})
	}
}

func TestExtractUnsignedIPAFindsOneApplication(t *testing.T) {
	ipa := writeDeployTestZIP(t, map[string]zipEntry{
		"Payload/":                   {mode: os.ModeDir | 0755},
		"Payload/App.app/":           {mode: os.ModeDir | 0755},
		"Payload/App.app/Info.plist": {data: "plist", mode: 0600},
		"Payload/App.app/App":        {data: "binary", mode: 0700},
	})
	root := filepath.Join(t.TempDir(), "out")
	app, err := extractUnsignedIPA(ipa, root)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(app) != "App.app" {
		t.Fatalf("app path = %q", app)
	}
}

func TestTakeAppleCredentialsUnsetsEnvironment(t *testing.T) {
	values := map[string]string{
		"APPLE_SIGNING_AGE_IDENTITY":      "AGE-SECRET-KEY-TEST",
		"APPLE_DISTRIBUTION_P12":          "cDEy",
		"APPLE_DISTRIBUTION_P12_PASSWORD": " password ",
		"APPLE_PROVISIONING_PROFILE":      "cHJvZmlsZQ==",
		"APPLE_PROVISIONING_PROFILES":     "cHJvZmlsZXM=",
		"APPLE_TEAM_ID":                   "TEAM123456",
		"ASC_API_KEY_P8":                  "key",
		"ASC_KEY_ID":                      "KEY1234567",
		"ASC_ISSUER_ID":                   "00000000-0000-0000-0000-000000000000",
	}
	for name, value := range values {
		t.Setenv(name, value)
	}
	credentials, err := takeAppleCredentials()
	if err != nil || credentials.teamID != "TEAM123456" || credentials.p12Password != " password " {
		t.Fatalf("takeAppleCredentials() = %#v, %v", credentials, err)
	}
	for name := range values {
		if value := os.Getenv(name); value != "" {
			t.Fatalf("%s remained in environment", name)
		}
	}
}

func TestTakeAppleCredentialsAcceptsProfileBundleWithoutLegacyProfile(t *testing.T) {
	values := map[string]string{
		"APPLE_SIGNING_AGE_IDENTITY":      "AGE-SECRET-KEY-TEST",
		"APPLE_DISTRIBUTION_P12":          "cDEy",
		"APPLE_DISTRIBUTION_P12_PASSWORD": "password",
		"APPLE_PROVISIONING_PROFILE":      "",
		"APPLE_PROVISIONING_PROFILES":     "cHJvZmlsZXM=",
		"APPLE_TEAM_ID":                   "TEAM123456",
		"ASC_API_KEY_P8":                  "key",
		"ASC_KEY_ID":                      "KEY1234567",
		"ASC_ISSUER_ID":                   "",
	}
	for name, value := range values {
		t.Setenv(name, value)
	}
	credentials, err := takeAppleCredentials()
	if err != nil || credentials.profile != "" || credentials.profiles == "" {
		t.Fatalf("takeAppleCredentials() = %#v, %v", credentials, err)
	}
}

func TestMaterializeProvisioningProfilesSupportsLegacyAndBundle(t *testing.T) {
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for name, contents := range map[string]string{
		"TripNearby.mobileprovision":     "trip profile",
		"GravityKingdom.mobileprovision": "gravity profile",
	} {
		member, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := member.Write([]byte(contents)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	paths, err := materializeProvisioningProfiles(
		base64.StdEncoding.EncodeToString([]byte("legacy profile")),
		base64.StdEncoding.EncodeToString(archive.Bytes()),
		destination,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 3 {
		t.Fatalf("profile paths = %#v", paths)
	}
	for _, profilePath := range paths {
		info, err := os.Stat(profilePath)
		if err != nil {
			t.Fatalf("stat profile %q: %v", profilePath, err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
			t.Fatalf("profile %q mode = %v", profilePath, info.Mode())
		}
	}
}

func TestMaterializeProvisioningProfilesRejectsUnsafeBundleMember(t *testing.T) {
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	member, err := writer.Create("profiles/App.mobileprovision")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := member.Write([]byte("profile")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = materializeProvisioningProfiles("", base64.StdEncoding.EncodeToString(archive.Bytes()), t.TempDir())
	if err == nil {
		t.Fatal("nested profile bundle member was accepted")
	}
}

func TestSelectProvisioningProfileUsesExactBundleAndNewestUsableProfile(t *testing.T) {
	certificate := []byte("certificate DER")
	fingerprint := sha1.Sum(certificate) // #nosec G401 -- mirrors Apple's certificate fingerprint.
	identity := strings.ToUpper(hex.EncodeToString(fingerprint[:]))
	profile := func(name, bundleID string, expires time.Time, certificate []byte) provisioningProfileCandidate {
		return provisioningProfileCandidate{
			path: name + ".mobileprovision",
			profile: provisioningProfile{
				UUID:                  name,
				Name:                  name,
				ExpirationDate:        expires,
				Platform:              []string{"iOS"},
				TeamIdentifier:        []string{"TEAM123456"},
				DeveloperCertificates: [][]byte{certificate},
				Entitlements: map[string]any{
					"application-identifier": "TEAM123456." + bundleID,
					"get-task-allow":         false,
				},
			},
		}
	}
	now := time.Now()
	candidates := []provisioningProfileCandidate{
		profile("other-app", "com.example.other", now.Add(72*time.Hour), certificate),
		profile("expired", "com.example.app", now.Add(-time.Hour), certificate),
		profile("wrong-certificate", "com.example.app", now.Add(96*time.Hour), []byte("other")),
		profile("selected", "com.example.app", now.Add(48*time.Hour), certificate),
	}
	selected, err := selectProvisioningProfile(candidates, "TEAM123456", "com.example.app", identity)
	if err != nil {
		t.Fatal(err)
	}
	if selected.profile.Name != "selected" {
		t.Fatalf("selected profile = %q", selected.profile.Name)
	}
	if _, err := selectProvisioningProfile(candidates, "TEAM123456", "com.example.missing", identity); err == nil {
		t.Fatal("missing Bundle ID unexpectedly selected a profile")
	}
}

func TestExecuteTestFlightEncryptsCredentialFailure(t *testing.T) {
	for _, name := range []string{
		"APPLE_SIGNING_AGE_IDENTITY", "APPLE_DISTRIBUTION_P12", "APPLE_DISTRIBUTION_P12_PASSWORD",
		"APPLE_PROVISIONING_PROFILE", "APPLE_TEAM_ID", "ASC_API_KEY_P8", "ASC_KEY_ID", "ASC_ISSUER_ID",
	} {
		t.Setenv(name, "")
	}
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	options := &TestFlightOptions{
		EncryptedIPAPath: filepath.Join(root, "intermediate", "App.ipa.age"),
		LogPath:          filepath.Join(root, "private-output", "build.log"),
		BuildNumber:      "42.1",
	}
	output := filepath.Join(root, "encrypted")
	err = ExecuteTestFlight(t.Context(), options, identity.Recipient().String(), output)
	if !errors.Is(err, ErrDeployFailed) {
		t.Fatalf("ExecuteTestFlight() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(output, "build.log.age")); err != nil {
		t.Fatalf("encrypted diagnostic missing: %v", err)
	}
	if _, err := os.Stat(options.LogPath); !os.IsNotExist(err) {
		t.Fatalf("plaintext diagnostic remained: %v", err)
	}
}

type zipEntry struct {
	data string
	mode os.FileMode
}

func writeDeployTestZIP(t *testing.T, entries map[string]zipEntry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "App.ipa")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	for name, entry := range entries {
		header := &zip.FileHeader{Name: name, Method: zip.Store}
		header.SetMode(entry.mode)
		member, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := member.Write([]byte(entry.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWriteTextOrBase64Secret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "key.p8")
	if err := writeTextOrBase64Secret("-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----", path, "PRIVATE KEY"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "BEGIN PRIVATE KEY") {
		t.Fatalf("key = %q, %v", data, err)
	}
}

func TestSetBundleBuildNumber(t *testing.T) {
	infoPath := filepath.Join(t.TempDir(), "Info.plist")
	original, err := plist.Marshal(map[string]any{
		"CFBundleIdentifier": "com.example.app",
		"CFBundleVersion":    "7",
	}, plist.XMLFormat)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(infoPath, original, 0644); err != nil {
		t.Fatal(err)
	}
	if err := setBundleBuildNumber(infoPath, "108.2"); err != nil {
		t.Fatal(err)
	}
	updated, err := os.ReadFile(infoPath)
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if _, err := plist.Unmarshal(updated, &values); err != nil {
		t.Fatal(err)
	}
	if values["CFBundleVersion"] != "108.2" || values["CFBundleIdentifier"] != "com.example.app" {
		t.Fatalf("updated Info.plist = %#v", values)
	}
}

func TestTestFlightBuildNumberValidation(t *testing.T) {
	root := t.TempDir()
	for _, number := range []string{"", "0", "01", "1.", "1.a", "1.2.3.4"} {
		options := &TestFlightOptions{
			EncryptedIPAPath: filepath.Join(root, "intermediate", "App.ipa.age"),
			LogPath:          filepath.Join(root, "private-output", "build.log"),
			BuildNumber:      number,
		}
		if err := options.validate(); err == nil {
			t.Fatalf("build number %q was accepted", number)
		}
	}
	for _, number := range []string{"1", "42.1", "9999.99.99"} {
		options := &TestFlightOptions{
			EncryptedIPAPath: filepath.Join(root, "intermediate", "App.ipa.age"),
			LogPath:          filepath.Join(root, "private-output", "build.log"),
			BuildNumber:      number,
		}
		if err := options.validate(); err != nil {
			t.Fatalf("build number %q rejected: %v", number, err)
		}
	}
}

func TestRejectNestedApplicationsFailClosed(t *testing.T) {
	for _, relative := range []string{
		"PlugIns/Share.appex", "Watch", "AppClips", "XPCServices/Service.xpc", "Nested.App",
	} {
		t.Run(relative, func(t *testing.T) {
			app := filepath.Join(t.TempDir(), "App.app")
			if err := os.MkdirAll(filepath.Join(app, relative), 0700); err != nil {
				t.Fatal(err)
			}
			if err := rejectNestedApplications(app); err == nil {
				t.Fatalf("nested bundle %q was accepted", relative)
			}
		})
	}
}

func TestAltoolArgsSupportsTeamAndIndividualKeys(t *testing.T) {
	team := altoolArgs("--validate-app", "/tmp/App.ipa", &appleCredentials{apiKeyID: "KEY1234567", issuerID: "issuer"})
	if !strings.Contains(strings.Join(team, " "), "--apiIssuer issuer") {
		t.Fatalf("team args = %#v", team)
	}
	individual := altoolArgs("--upload-app", "/tmp/App.ipa", &appleCredentials{apiKeyID: "KEY1234567"})
	if strings.Contains(strings.Join(individual, " "), "--apiIssuer") {
		t.Fatalf("individual args = %#v", individual)
	}
}
