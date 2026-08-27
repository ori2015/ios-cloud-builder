package runner

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1" // #nosec G505 -- Apple profile certificate identifiers are SHA-1 fingerprints, not signatures.
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"filippo.io/age"
	"howett.net/plist"
)

const (
	maxDeployIPABytes = int64(1024 * 1024 * 1024)
	maxDeployEntries  = 100000
	maxSecretBytes    = 2 * 1024 * 1024
	maxProfiles       = 100
	maxProfileBytes   = 512 * 1024
	autoBuildNumber   = "auto"
)

var (
	ErrDeployFailed = errors.New("private TestFlight deployment failed; download the encrypted diagnostic log")
	ErrAdHocFailed  = errors.New("private ad hoc signing failed; download the encrypted diagnostic log")
	appleIDPattern  = regexp.MustCompile(`^[A-Z0-9]{10}$`)
	issuerPattern   = regexp.MustCompile(`^[0-9a-fA-F-]{36}$`)
	identityPattern = regexp.MustCompile(`(?m)^\s*[0-9]+\)\s+([0-9A-Fa-f]{40})\s+"([^"\r\n]+)"`)
	buildPattern    = regexp.MustCompile(`^[1-9][0-9]{0,17}(?:\.[1-9][0-9]{0,17}){0,2}$`)
)

// TestFlightOptions identifies only trusted-runner temporary files. Secret
// values are read from and immediately removed from the process environment.
type TestFlightOptions struct {
	EncryptedIPAPath string
	ManifestPath     string
	LogPath          string
	BuildNumber      string
	Expected         ProvenanceExpectation
}

type appleCredentials struct {
	p12         string
	p12Password string
	profile     string
	profiles    string
	teamID      string
	apiKey      string
	apiKeyID    string
	issuerID    string
	betaGroups  string
}

type provisioningProfile struct {
	UUID                  string         `plist:"UUID"`
	Name                  string         `plist:"Name"`
	ExpirationDate        time.Time      `plist:"ExpirationDate"`
	Platform              []string       `plist:"Platform"`
	TeamIdentifier        []string       `plist:"TeamIdentifier"`
	DeveloperCertificates [][]byte       `plist:"DeveloperCertificates"`
	ProvisionedDevices    []string       `plist:"ProvisionedDevices"`
	ProvisionsAllDevices  bool           `plist:"ProvisionsAllDevices"`
	Entitlements          map[string]any `plist:"Entitlements"`
}

type provisioningProfileCandidate struct {
	path    string
	profile provisioningProfile
}

// ExecuteTestFlight decrypts an authenticated unsigned IPA, signs it without
// executing private project code, uploads it, encrypts diagnostics for the
// local CLI, and removes every plaintext and credential file.
func ExecuteTestFlight(ctx context.Context, options *TestFlightOptions, recipient, encryptedDir string) error {
	if err := options.validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(options.LogPath), 0700); err != nil {
		return fmt.Errorf("prepare private deployment output")
	}
	logFile, err := os.OpenFile(options.LogPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("prepare private deployment log")
	}
	manifest, provenanceErr := ValidateProvenanceArtifact(filepath.Dir(options.EncryptedIPAPath), options.Expected)
	identity, identityErr := takeTransportIdentity()
	deployErr := provenanceErr
	if deployErr == nil {
		deployErr = identityErr
	}
	if deployErr == nil {
		deployErr = deployTestFlight(ctx, options, manifest, identity, logFile)
	}
	if deployErr != nil {
		_, _ = fmt.Fprintf(logFile, "\nDeployment failed: %v\n", deployErr)
	}
	_ = logFile.Sync()
	_ = logFile.Close()
	if err := EncryptArtifacts(recipient, options.LogPath, "", encryptedDir); err != nil {
		return fmt.Errorf("encrypt private deployment diagnostics")
	}
	if deployErr != nil {
		return ErrDeployFailed
	}
	return nil
}

// AdHocOptions mirrors TestFlightOptions for the ad hoc path. The signed IPA is
// encrypted to the calling project's own recipient — unlike TestFlight, where
// the signed artifact never leaves the protected environment.
type AdHocOptions struct {
	EncryptedIPAPath string
	ManifestPath     string
	LogPath          string
	Expected         ProvenanceExpectation
}

func (options *AdHocOptions) validate() error {
	if options == nil || !filepath.IsAbs(options.EncryptedIPAPath) || !filepath.IsAbs(options.LogPath) {
		return fmt.Errorf("ad hoc signing paths must be absolute")
	}
	if filepath.Base(options.EncryptedIPAPath) != trustedIPAFile || filepath.Base(options.ManifestPath) != provenanceFile ||
		filepath.Base(options.LogPath) != "build.log" ||
		filepath.Dir(options.EncryptedIPAPath) != filepath.Dir(options.ManifestPath) || options.Expected.validate() != nil {
		return fmt.Errorf("invalid ad hoc signing paths")
	}
	return nil
}

// ExecuteAdHoc decrypts an authenticated unsigned IPA, signs it with an ad hoc
// distribution profile without executing private project code, and re-encrypts
// the signed IPA to the calling project's recipient so it can be installed
// directly on registered devices.
func ExecuteAdHoc(ctx context.Context, options *AdHocOptions, recipient, encryptedDir string) error {
	if err := options.validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(options.LogPath), 0700); err != nil {
		return fmt.Errorf("prepare private ad hoc output")
	}
	logFile, err := os.OpenFile(options.LogPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("prepare private ad hoc log")
	}
	manifest, provenanceErr := ValidateProvenanceArtifact(filepath.Dir(options.EncryptedIPAPath), options.Expected)
	identity, identityErr := takeTransportIdentity()
	signErr := provenanceErr
	if signErr == nil {
		signErr = identityErr
	}
	var signedIPA string
	if signErr == nil {
		signedIPA, signErr = signAdHoc(ctx, options, manifest, identity, logFile)
	}
	if signErr != nil {
		_, _ = fmt.Fprintf(logFile, "\nAd hoc signing failed: %v\n", signErr)
	}
	_ = logFile.Sync()
	_ = logFile.Close()
	// Encrypts both to the caller and removes the plaintexts, exactly as the
	// unsigned build path does — the difference is only that this IPA is signed.
	if err := EncryptArtifacts(recipient, options.LogPath, signedIPA, encryptedDir); err != nil {
		return fmt.Errorf("encrypt private ad hoc artifacts")
	}
	if signErr != nil {
		return ErrAdHocFailed
	}
	return nil
}

func signAdHoc(ctx context.Context, options *AdHocOptions, manifest *ProvenanceManifest, identity age.Identity, privateLog io.Writer) (string, error) {
	workRoot, err := os.MkdirTemp(filepath.Dir(options.LogPath), ".adhoc-")
	if err != nil {
		return "", fmt.Errorf("prepare ad hoc workspace")
	}
	privateHome := filepath.Join(workRoot, "home")
	if err := os.Mkdir(privateHome, 0700); err != nil {
		return "", fmt.Errorf("prepare isolated ad hoc home")
	}
	signingHome := strings.TrimSpace(os.Getenv("HOME"))
	if !filepath.IsAbs(signingHome) {
		return "", fmt.Errorf("prepare signing environment")
	}
	run := executor{ctx: ctx, env: ChildEnvironment(workRoot, signingHome), log: privateLog}

	unsignedIPA := filepath.Join(workRoot, "unsigned.ipa")
	if err := decryptFileBounded(identity, options.EncryptedIPAPath, unsignedIPA); err != nil {
		return "", err
	}
	if err := verifyPlaintextIPADigest(unsignedIPA, manifest); err != nil {
		return "", fmt.Errorf("unsigned IPA does not match authenticated provenance")
	}
	payloadRoot := filepath.Join(workRoot, "unsigned")
	appPath, err := extractUnsignedIPA(unsignedIPA, payloadRoot)
	_ = os.Remove(unsignedIPA)
	if err != nil {
		return "", err
	}
	if err := rejectNestedApplications(appPath); err != nil {
		return "", err
	}
	bundleID, _, err := readAppMetadata(filepath.Join(appPath, "Info.plist"))
	if err != nil {
		return "", err
	}
	credentials, err := takeAppleCredentials()
	if err != nil {
		return "", err
	}

	secretsDir := filepath.Join(workRoot, "credentials")
	if err := os.Mkdir(secretsDir, 0700); err != nil {
		return "", fmt.Errorf("prepare credential files")
	}
	p12Path := filepath.Join(secretsDir, "distribution.p12")
	if err := writeBase64Secret(credentials.p12, p12Path); err != nil {
		return "", fmt.Errorf("decode distribution certificate")
	}
	if err := signApplicationInPlace(ctx, &signRequest{
		run:         run,
		workRoot:    workRoot,
		secretsDir:  secretsDir,
		privateHome: privateHome,
		appPath:     appPath,
		bundleID:    bundleID,
		p12Path:     p12Path,
		profileType: ascAdHocProfileType,
		credentials: credentials,
		privateLog:  privateLog,
	}); err != nil {
		return "", err
	}

	// Written outside workRoot: the caller encrypts it after this returns, and
	// the workspace is removed as soon as signing finishes.
	signedIPA := filepath.Join(filepath.Dir(options.LogPath), "App.ipa")
	if err := run.run(payloadRoot, "/usr/bin/ditto", "-c", "-k", "--sequesterRsrc", "--keepParent", "Payload", signedIPA); err != nil {
		_ = os.RemoveAll(workRoot)
		return "", err
	}
	_ = os.RemoveAll(workRoot)
	info, err := os.Stat(signedIPA)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return "", fmt.Errorf("signed IPA packaging produced no output")
	}
	_, _ = fmt.Fprintf(privateLog, "Signed ad hoc IPA for %s (%d bytes).\n", bundleID, info.Size())
	return signedIPA, nil
}

func (options *TestFlightOptions) validate() error {
	if options == nil || !filepath.IsAbs(options.EncryptedIPAPath) || !filepath.IsAbs(options.LogPath) {
		return fmt.Errorf("deployment paths must be absolute")
	}
	if filepath.Base(options.EncryptedIPAPath) != trustedIPAFile || filepath.Base(options.ManifestPath) != provenanceFile || filepath.Base(options.LogPath) != "build.log" ||
		filepath.Dir(options.EncryptedIPAPath) != filepath.Dir(options.ManifestPath) || options.Expected.validate() != nil {
		return fmt.Errorf("invalid deployment paths")
	}
	if options.BuildNumber != autoBuildNumber && !buildPattern.MatchString(options.BuildNumber) {
		return fmt.Errorf("invalid TestFlight build number")
	}
	return nil
}

func takeAppleCredentials() (*appleCredentials, error) {
	takeRaw := func(name string) string {
		value := os.Getenv(name)
		_ = os.Unsetenv(name)
		return value
	}
	take := func(name string) string { return strings.TrimSpace(takeRaw(name)) }
	credentials := &appleCredentials{
		p12:         take("APPLE_DISTRIBUTION_P12"),
		p12Password: takeRaw("APPLE_DISTRIBUTION_P12_PASSWORD"),
		profile:     take("APPLE_PROVISIONING_PROFILE"),
		profiles:    take("APPLE_PROVISIONING_PROFILES"),
		teamID:      take("APPLE_TEAM_ID"),
		apiKey:      take("ASC_API_KEY_P8"),
		apiKeyID:    take("ASC_KEY_ID"),
		issuerID:    take("ASC_ISSUER_ID"),
		betaGroups:  take("TESTFLIGHT_BETA_GROUPS"),
	}
	if credentials.p12 == "" || credentials.p12Password == "" ||
		(credentials.profile == "" && credentials.profiles == "" && credentials.issuerID == "") ||
		credentials.teamID == "" || credentials.apiKey == "" ||
		credentials.apiKeyID == "" {
		return nil, fmt.Errorf("protected Apple environment is incomplete")
	}
	if !appleIDPattern.MatchString(credentials.teamID) || !appleIDPattern.MatchString(credentials.apiKeyID) ||
		(credentials.issuerID != "" && !issuerPattern.MatchString(credentials.issuerID)) {
		return nil, fmt.Errorf("protected Apple environment contains invalid identifiers")
	}
	return credentials, nil
}

func takeTransportIdentity() (age.Identity, error) {
	value := strings.TrimSpace(os.Getenv("APPLE_SIGNING_AGE_IDENTITY"))
	_ = os.Unsetenv("APPLE_SIGNING_AGE_IDENTITY")
	identity, err := age.ParseX25519Identity(value)
	if err != nil {
		return nil, fmt.Errorf("parse signing transport identity")
	}
	return identity, nil
}

func deployTestFlight(ctx context.Context, options *TestFlightOptions, manifest *ProvenanceManifest, identity age.Identity, privateLog io.Writer) error {
	workRoot, err := os.MkdirTemp(filepath.Dir(options.LogPath), ".testflight-")
	if err != nil {
		return fmt.Errorf("prepare deployment workspace")
	}
	defer func() { _ = os.RemoveAll(workRoot) }()
	privateHome := filepath.Join(workRoot, "home")
	if err := os.Mkdir(privateHome, 0700); err != nil {
		return fmt.Errorf("prepare isolated deployment home")
	}
	signingHome := strings.TrimSpace(os.Getenv("HOME"))
	if !filepath.IsAbs(signingHome) {
		return fmt.Errorf("prepare signing environment")
	}
	run := executor{ctx: ctx, env: ChildEnvironment(workRoot, signingHome), log: privateLog}
	uploadRun := executor{ctx: ctx, env: ChildEnvironment(workRoot, privateHome), log: privateLog}

	unsignedIPA := filepath.Join(workRoot, "unsigned.ipa")
	if err := decryptFileBounded(identity, options.EncryptedIPAPath, unsignedIPA); err != nil {
		return err
	}
	if err := verifyPlaintextIPADigest(unsignedIPA, manifest); err != nil {
		return fmt.Errorf("unsigned IPA does not match authenticated provenance")
	}
	payloadRoot := filepath.Join(workRoot, "unsigned")
	appPath, err := extractUnsignedIPA(unsignedIPA, payloadRoot)
	_ = os.Remove(unsignedIPA)
	if err != nil {
		return err
	}
	if err := rejectNestedApplications(appPath); err != nil {
		return err
	}
	infoPath := filepath.Join(appPath, "Info.plist")
	bundleID, marketingVersion, err := readAppMetadata(infoPath)
	if err != nil {
		return err
	}
	credentials, err := takeAppleCredentials()
	if err != nil {
		return err
	}
	buildNumber := options.BuildNumber
	var publisher *appStoreConnectClient
	if buildNumber == autoBuildNumber {
		if credentials.issuerID == "" {
			return fmt.Errorf("automatic TestFlight build numbering requires an App Store Connect issuer ID")
		}
		publisher, err = newAppStoreConnectClient(credentials.apiKeyID, credentials.issuerID, credentials.apiKey)
		if err != nil {
			return err
		}
		buildNumber, err = nextASCBuildNumber(ctx, publisher, bundleID, marketingVersion)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(privateLog, "Selected TestFlight build number %s after inspecting App Store Connect.\n", buildNumber)
	}
	if err := setBundleBuildNumber(infoPath, buildNumber); err != nil {
		return err
	}
	betaGroup, err := betaGroupForBundle(credentials.betaGroups, bundleID)
	if err != nil {
		return err
	}

	secretsDir := filepath.Join(workRoot, "credentials")
	if err := os.Mkdir(secretsDir, 0700); err != nil {
		return fmt.Errorf("prepare credential files")
	}
	p12Path := filepath.Join(secretsDir, "distribution.p12")
	apiKeyPath := filepath.Join(workRoot, "private_keys", "AuthKey_"+credentials.apiKeyID+".p8")
	if err := writeBase64Secret(credentials.p12, p12Path); err != nil {
		return fmt.Errorf("decode distribution certificate")
	}
	if err := writeTextOrBase64Secret(credentials.apiKey, apiKeyPath, "PRIVATE KEY"); err != nil {
		return fmt.Errorf("decode App Store Connect key")
	}

	if err := signApplicationInPlace(ctx, &signRequest{
		run:         run,
		workRoot:    workRoot,
		secretsDir:  secretsDir,
		privateHome: privateHome,
		appPath:     appPath,
		bundleID:    bundleID,
		p12Path:     p12Path,
		profileType: ascAppStoreProfileType,
		credentials: credentials,
		publisher:   publisher,
		privateLog:  privateLog,
	}); err != nil {
		return err
	}

	signedIPA := filepath.Join(workRoot, "App.ipa")
	if err := run.run(payloadRoot, "/usr/bin/ditto", "-c", "-k", "--sequesterRsrc", "--keepParent", "Payload", signedIPA); err != nil {
		return err
	}
	info, err := os.Stat(signedIPA)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("signed IPA packaging produced no output")
	}
	fmt.Println("Validating application with App Store Connect...")
	if err := uploadRun.runSensitive(workRoot, "/usr/bin/xcrun", altoolArgs("--validate-app", signedIPA, credentials)...); err != nil {
		return fmt.Errorf("validation with App Store Connect failed")
	}
	fmt.Println("Uploading application to App Store Connect...")
	if err := uploadRun.runSensitive(workRoot, "/usr/bin/xcrun", altoolArgs("--upload-app", signedIPA, credentials)...); err != nil {
		return fmt.Errorf("upload to App Store Connect failed")
	}
	fmt.Println("App Store Connect accepted the upload. Waiting for it to finish processing the build...")
	_, _ = fmt.Fprintln(privateLog, "App Store Connect accepted the signed IPA upload.")
	if credentials.issuerID == "" {
		return fmt.Errorf("TestFlight processing verification requires an App Store Connect issuer ID")
	}
	if publisher == nil {
		publisher, err = newAppStoreConnectClient(credentials.apiKeyID, credentials.issuerID, credentials.apiKey)
		if err != nil {
			return err
		}
	}
	appID, processedBuild, err := waitForUploadedASCBuild(ctx, publisher, bundleID, marketingVersion, buildNumber, privateLog)
	if err != nil {
		return err
	}
	if betaGroup != nil {
		if err := publishProcessedASCBuildToBetaGroup(ctx, publisher, appID, processedBuild, &betaPublishRequest{
			BundleID:         bundleID,
			MarketingVersion: marketingVersion,
			BuildNumber:      buildNumber,
			Group:            *betaGroup,
		}, privateLog); err != nil {
			return err
		}
	}
	return nil
}

// signRequest carries everything the shared signing step needs. It exists so
// TestFlight and ad hoc signing cannot drift apart: both import the same
// identity into the same kind of disposable keychain and both pick a profile
// through the same validated selection, differing only in profileType.
type signRequest struct {
	run         executor
	workRoot    string
	secretsDir  string
	privateHome string
	appPath     string
	bundleID    string
	p12Path     string
	profileType string
	credentials *appleCredentials
	publisher   *appStoreConnectClient
	privateLog  io.Writer
}

// signApplicationInPlace imports the distribution identity, selects or creates a
// matching provisioning profile, embeds it, and code-signs the application at
// req.appPath. The keychain is destroyed before it returns.
func signApplicationInPlace(ctx context.Context, req *signRequest) error {
	run, workRoot, credentials := req.run, req.workRoot, req.credentials
	keychainPath := filepath.Join(workRoot, "signing.keychain-db")
	keychainPassword, err := randomPassword()
	if err != nil {
		return fmt.Errorf("create temporary keychain password")
	}
	defer func() { _ = run.runSensitive(workRoot, "/usr/bin/security", "delete-keychain", keychainPath) }()
	if err := run.runSensitive(workRoot, "/usr/bin/security", "create-keychain", "-p", keychainPassword, keychainPath); err != nil {
		return err
	}
	if err := run.runSensitive(workRoot, "/usr/bin/security", "set-keychain-settings", "-lut", "21600", keychainPath); err != nil {
		return err
	}
	if err := run.runSensitive(workRoot, "/usr/bin/security", "unlock-keychain", "-p", keychainPassword, keychainPath); err != nil {
		return err
	}
	if err := run.runSensitive(workRoot, "/usr/bin/security", "import", req.p12Path, "-P", credentials.p12Password,
		"-T", "/usr/bin/codesign", "-t", "cert", "-f", "pkcs12", "-k", keychainPath); err != nil {
		return err
	}
	if err := run.runSensitive(workRoot, "/usr/bin/security", "set-key-partition-list", "-S", "apple-tool:,apple:,codesign:", "-s", "-k", keychainPassword, keychainPath); err != nil {
		return err
	}
	if err := run.runSensitive(workRoot, "/usr/bin/security", "list-keychains", "-d", "user", "-s", keychainPath); err != nil {
		return err
	}
	// Keep code-signing identity discovery scoped to the disposable keychain.
	identityOutput, err := run.capture(workRoot, "/usr/bin/security", "find-identity", "-v", "-p", "codesigning", keychainPath)
	if err != nil {
		return err
	}
	identityMatch := identityPattern.FindStringSubmatch(string(identityOutput))
	if len(identityMatch) != 3 || !strings.HasPrefix(identityMatch[2], "Apple Distribution: ") ||
		!strings.HasSuffix(identityMatch[2], "("+credentials.teamID+")") {
		return fmt.Errorf("distribution signing identity was not imported")
	}
	identityFingerprint, signingIdentity := identityMatch[1], identityMatch[2]

	profilePaths, err := materializeProvisioningProfiles(credentials.profile, credentials.profiles, req.secretsDir)
	if err != nil {
		return err
	}
	publisher := req.publisher
	var bundleResourceID string
	if credentials.issuerID != "" {
		if publisher == nil {
			publisher, err = newAppStoreConnectClient(credentials.apiKeyID, credentials.issuerID, credentials.apiKey)
			if err != nil {
				return err
			}
		}
		var downloaded []string
		bundleResourceID, downloaded, err = downloadASCProvisioningProfiles(ctx, publisher, req.bundleID, req.secretsDir, req.profileType)
		if err != nil {
			if len(profilePaths) == 0 {
				return err
			}
			_, _ = fmt.Fprintf(req.privateLog, "App Store Connect profile discovery failed: %v\nTrying protected fallback profiles.\n", err)
		} else {
			profilePaths = append(profilePaths, downloaded...)
		}
	}

	parseCandidate := func(candidatePath string) (provisioningProfileCandidate, error) {
		profileOutput, captureErr := run.capture(workRoot, "/usr/bin/security", "cms", "-D", "-i", candidatePath, "-k", keychainPath)
		if captureErr != nil {
			return provisioningProfileCandidate{}, captureErr
		}
		var parsed provisioningProfile
		if _, unmarshalErr := plist.Unmarshal(profileOutput, &parsed); unmarshalErr != nil {
			return provisioningProfileCandidate{}, fmt.Errorf("parse provisioning profile bundle")
		}
		return provisioningProfileCandidate{path: candidatePath, profile: parsed}, nil
	}

	candidates := make([]provisioningProfileCandidate, 0, len(profilePaths))
	for _, candidatePath := range profilePaths {
		candidate, parseErr := parseCandidate(candidatePath)
		if parseErr != nil {
			return parseErr
		}
		candidates = append(candidates, candidate)
	}
	selected, selectionErr := selectProvisioningProfile(candidates, credentials.teamID, req.bundleID, identityFingerprint, req.profileType)
	if selectionErr != nil && publisher != nil && bundleResourceID != "" {
		createdPath, createErr := createASCProvisioningProfile(ctx, publisher, bundleResourceID, req.bundleID, identityFingerprint, req.secretsDir, req.profileType)
		if createErr != nil {
			return createErr
		}
		created, parseErr := parseCandidate(createdPath)
		if parseErr != nil {
			return parseErr
		}
		candidates = append(candidates, created)
		selected, selectionErr = selectProvisioningProfile(candidates, credentials.teamID, req.bundleID, identityFingerprint, req.profileType)
		if selectionErr == nil {
			_, _ = fmt.Fprintf(req.privateLog, "Created a %s provisioning profile through the App Store Connect API.\n", req.profileType)
		}
	}
	if selectionErr != nil {
		return selectionErr
	}
	profilePath, profile := selected.path, selected.profile
	if req.profileType == ascAdHocProfileType {
		_, _ = fmt.Fprintf(req.privateLog, "Ad hoc profile %q provisions %d device(s).\n", profile.Name, len(profile.ProvisionedDevices))
	}
	installedProfile := filepath.Join(req.privateHome, "Library", "MobileDevice", "Provisioning Profiles", profile.UUID+".mobileprovision")
	if err := copyPrivateFile(profilePath, installedProfile); err != nil {
		return fmt.Errorf("install provisioning profile")
	}
	if err := copyPrivateFile(profilePath, filepath.Join(req.appPath, "embedded.mobileprovision")); err != nil {
		return fmt.Errorf("embed provisioning profile")
	}
	entitlementsPath := filepath.Join(req.secretsDir, "entitlements.plist")
	entitlements, err := plist.Marshal(profile.Entitlements, plist.XMLFormat)
	if err != nil || os.WriteFile(entitlementsPath, entitlements, 0600) != nil {
		return fmt.Errorf("prepare signing entitlements")
	}
	return signApplication(run, req.appPath, signingIdentity, entitlementsPath, keychainPath)
}

func (e executor) runSensitive(dir, program string, args ...string) error {
	_, _ = fmt.Fprintf(e.log, "\n$ %s [arguments redacted]\n", filepath.Base(program))
	cmd := exec.CommandContext(e.ctx, program, args...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, e.env, e.log, e.log
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s failed: %w", filepath.Base(program), err)
	}
	return nil
}

// decryptFileBounded caps the plaintext at maxDeployIPABytes; every caller
// decrypts an unsigned IPA, so the bound is the same for all of them.
func decryptFileBounded(identity age.Identity, sourcePath, destinationPath string) error {
	const limit = maxDeployIPABytes
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open encrypted unsigned IPA")
	}
	defer source.Close()
	reader, err := age.Decrypt(source, identity)
	if err != nil {
		return fmt.Errorf("decrypt unsigned IPA")
	}
	destination, err := os.OpenFile(destinationPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("prepare unsigned IPA")
	}
	defer destination.Close()
	written, err := io.Copy(destination, io.LimitReader(reader, limit+1))
	if err != nil || written == 0 || written > limit {
		_ = os.Remove(destinationPath)
		return fmt.Errorf("invalid unsigned IPA ciphertext")
	}
	return nil
}

func extractUnsignedIPA(ipaPath, destinationRoot string) (string, error) {
	reader, err := zip.OpenReader(ipaPath)
	if err != nil {
		return "", fmt.Errorf("open unsigned IPA")
	}
	defer func() { _ = reader.Close() }()
	if len(reader.File) == 0 || len(reader.File) > maxDeployEntries {
		return "", fmt.Errorf("invalid unsigned IPA entry count")
	}
	var total uint64
	for _, entry := range reader.File {
		rawName := entry.Name
		name := strings.TrimSuffix(rawName, "/")
		clean := path.Clean(name)
		if name == "" || clean != name || strings.HasPrefix(rawName, "/") || strings.Contains(rawName, `\`) ||
			(clean != "Payload" && !strings.HasPrefix(clean, "Payload/")) || entry.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("unsafe unsigned IPA member")
		}
		if entry.UncompressedSize64 > uint64(maxDeployIPABytes*2) || total > uint64(maxDeployIPABytes*2)-entry.UncompressedSize64 {
			return "", fmt.Errorf("unsigned IPA expands beyond the allowed size")
		}
		total += entry.UncompressedSize64
		destination := filepath.Join(destinationRoot, filepath.FromSlash(clean))
		if !pathWithin(destinationRoot, destination) {
			return "", fmt.Errorf("unsigned IPA member escapes extraction root")
		}
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(destination, 0700); err != nil {
				return "", fmt.Errorf("extract unsigned IPA")
			}
			mode := entry.Mode().Perm() & 0755
			if mode == 0 {
				mode = 0755
			}
			if err := os.Chmod(destination, mode); err != nil {
				return "", fmt.Errorf("extract unsigned IPA")
			}
			continue
		}
		if !entry.Mode().IsRegular() {
			return "", fmt.Errorf("unsupported unsigned IPA member")
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
			return "", fmt.Errorf("extract unsigned IPA")
		}
		source, err := entry.Open()
		if err != nil {
			return "", fmt.Errorf("extract unsigned IPA")
		}
		mode := entry.Mode().Perm() & 0755
		if mode == 0 {
			mode = 0644
		}
		if entry.Mode().Perm()&0111 != 0 {
			mode |= 0111
		}
		target, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			source.Close()
			return "", fmt.Errorf("extract unsigned IPA")
		}
		_, copyErr := io.Copy(target, source)
		closeErr := target.Close()
		source.Close()
		if copyErr != nil || closeErr != nil {
			return "", fmt.Errorf("extract unsigned IPA")
		}
	}
	payload := filepath.Join(destinationRoot, "Payload")
	entries, err := os.ReadDir(payload)
	if err != nil {
		return "", fmt.Errorf("unsigned IPA has no Payload directory")
	}
	var apps []string
	for _, entry := range entries {
		if entry.IsDir() && strings.HasSuffix(entry.Name(), ".app") {
			apps = append(apps, filepath.Join(payload, entry.Name()))
		}
	}
	if len(apps) != 1 {
		return "", fmt.Errorf("unsigned IPA must contain exactly one application")
	}
	return apps[0], nil
}

func rejectNestedApplications(appPath string) error {
	return filepath.WalkDir(appPath, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == appPath {
			return nil
		}
		name := strings.ToLower(entry.Name())
		if entry.IsDir() && (strings.HasSuffix(name, ".appex") || strings.HasSuffix(name, ".app") ||
			strings.HasSuffix(name, ".xpc") || name == "plugins" || name == "watch" ||
			name == "appclips" || name == "xpcservices") {
			return fmt.Errorf("embedded applications require separate provisioning profiles and are not supported")
		}
		return nil
	})
}

func setBundleBuildNumber(infoPath, buildNumber string) error {
	if !buildPattern.MatchString(buildNumber) {
		return fmt.Errorf("invalid TestFlight build number")
	}
	info, err := os.Lstat(infoPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 4*1024*1024 {
		return fmt.Errorf("inspect application Info.plist")
	}
	data, err := os.ReadFile(infoPath)
	if err != nil {
		return fmt.Errorf("read application Info.plist")
	}
	var values map[string]any
	if _, err := plist.Unmarshal(data, &values); err != nil {
		return fmt.Errorf("parse application Info.plist")
	}
	if values == nil {
		return fmt.Errorf("parse application Info.plist")
	}
	values["CFBundleVersion"] = buildNumber
	updated, err := plist.Marshal(values, plist.BinaryFormat)
	if err != nil {
		return fmt.Errorf("update application build number")
	}
	if err := os.WriteFile(infoPath, updated, info.Mode().Perm()); err != nil {
		return fmt.Errorf("update application build number")
	}
	return nil
}

func altoolArgs(operation, ipaPath string, credentials *appleCredentials) []string {
	args := []string{"altool", operation, "-f", ipaPath, "-t", "ios", "--apiKey", credentials.apiKeyID}
	if credentials.issuerID != "" {
		args = append(args, "--apiIssuer", credentials.issuerID)
	}
	return args
}

func signApplication(run executor, appPath, identity, entitlementsPath, keychainPath string) error {
	var nested []string
	err := filepath.WalkDir(appPath, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == appPath {
			return nil
		}
		if entry.IsDir() && strings.HasSuffix(entry.Name(), ".framework") {
			nested = append(nested, path)
			return filepath.SkipDir
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".dylib") {
			nested = append(nested, path)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("inspect nested code")
	}
	sort.Slice(nested, func(i, j int) bool {
		return strings.Count(nested[i], string(filepath.Separator)) > strings.Count(nested[j], string(filepath.Separator))
	})
	for _, target := range nested {
		if err := run.run(filepath.Dir(target), "/usr/bin/codesign", "--force", "--sign", identity, "--keychain", keychainPath, "--timestamp=none", target); err != nil {
			return err
		}
	}
	if err := run.run(filepath.Dir(appPath), "/usr/bin/codesign", "--force", "--sign", identity, "--keychain", keychainPath, "--timestamp=none", "--generate-entitlement-der", "--entitlements", entitlementsPath, appPath); err != nil {
		return err
	}
	return run.run(filepath.Dir(appPath), "/usr/bin/codesign", "--verify", "--deep", "--strict", appPath)
}

func readAppMetadata(infoPath string) (string, string, error) {
	data, err := os.ReadFile(infoPath)
	if err != nil || len(data) > 4*1024*1024 {
		return "", "", fmt.Errorf("read application Info.plist")
	}
	var info struct {
		BundleID         string `plist:"CFBundleIdentifier"`
		MarketingVersion string `plist:"CFBundleShortVersionString"`
	}
	if _, err := plist.Unmarshal(data, &info); err != nil || info.BundleID == "" || info.MarketingVersion == "" ||
		strings.ContainsAny(info.BundleID+info.MarketingVersion, "\r\n\x00") {
		return "", "", fmt.Errorf("read application metadata")
	}
	return info.BundleID, info.MarketingVersion, nil
}

func profileMatchesBundle(applicationID, teamID, bundleID string) bool {
	prefix := teamID + "."
	return strings.HasPrefix(applicationID, prefix) && strings.TrimPrefix(applicationID, prefix) == bundleID
}

func materializeProvisioningProfiles(singleProfile, profileBundle, destinationDir string) ([]string, error) {
	var paths []string
	if singleProfile != "" {
		profilePath := filepath.Join(destinationDir, "profile-legacy.mobileprovision")
		if err := writeBase64Secret(singleProfile, profilePath); err != nil {
			return nil, fmt.Errorf("decode legacy provisioning profile")
		}
		paths = append(paths, profilePath)
	}
	if profileBundle == "" {
		return paths, nil
	}

	archive, err := decodeBase64Secret(profileBundle)
	if err != nil {
		return nil, fmt.Errorf("decode provisioning profile bundle")
	}
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil || len(reader.File) == 0 || len(reader.File) > maxProfiles {
		return nil, fmt.Errorf("invalid provisioning profile bundle")
	}
	var declaredTotal, actualTotal uint64
	for index, entry := range reader.File {
		clean := path.Clean(entry.Name)
		if clean != entry.Name || path.Base(clean) != clean || strings.HasPrefix(clean, ".") ||
			!strings.HasSuffix(strings.ToLower(clean), ".mobileprovision") || !entry.Mode().IsRegular() ||
			entry.UncompressedSize64 == 0 || entry.UncompressedSize64 > maxProfileBytes ||
			declaredTotal > uint64(maxSecretBytes)-entry.UncompressedSize64 {
			return nil, fmt.Errorf("invalid provisioning profile bundle member")
		}
		declaredTotal += entry.UncompressedSize64
		source, openErr := entry.Open()
		if openErr != nil {
			return nil, fmt.Errorf("read provisioning profile bundle")
		}
		data, readErr := io.ReadAll(io.LimitReader(source, maxProfileBytes+1))
		closeErr := source.Close()
		if readErr != nil || closeErr != nil || len(data) == 0 || len(data) > maxProfileBytes ||
			actualTotal > uint64(maxSecretBytes)-uint64(len(data)) {
			return nil, fmt.Errorf("read provisioning profile bundle")
		}
		actualTotal += uint64(len(data))
		profilePath := filepath.Join(destinationDir, fmt.Sprintf("profile-%03d.mobileprovision", index))
		if err := writePrivateFile(profilePath, data); err != nil {
			return nil, fmt.Errorf("write provisioning profile bundle")
		}
		paths = append(paths, profilePath)
	}
	return paths, nil
}

func selectProvisioningProfile(candidates []provisioningProfileCandidate, teamID, bundleID, identityFingerprint, profileType string) (provisioningProfileCandidate, error) {
	var matching []provisioningProfileCandidate
	for index := range candidates {
		candidate := &candidates[index]
		profile := candidate.profile
		if profile.UUID == "" || profile.Name == "" || len(profile.TeamIdentifier) != 1 || profile.TeamIdentifier[0] != teamID {
			return provisioningProfileCandidate{}, fmt.Errorf("provisioning profile bundle does not match APPLE_TEAM_ID")
		}
		applicationID, ok := profile.Entitlements["application-identifier"].(string)
		if !ok || applicationID == "" {
			return provisioningProfileCandidate{}, fmt.Errorf("provisioning profile bundle has no application identifier")
		}
		if profileMatchesBundle(applicationID, teamID, bundleID) {
			matching = append(matching, *candidate)
		}
	}
	if len(matching) == 0 {
		return provisioningProfileCandidate{}, fmt.Errorf("no provisioning profile matches the application bundle identifier")
	}
	sort.SliceStable(matching, func(i, j int) bool {
		return matching[i].profile.ExpirationDate.After(matching[j].profile.ExpirationDate)
	})
	for index := range matching {
		candidate := &matching[index]
		if validateDistributionProfile(&candidate.profile, profileType) == nil &&
			profileAuthorizesIdentity(candidate.profile.DeveloperCertificates, identityFingerprint) {
			return *candidate, nil
		}
	}
	return provisioningProfileCandidate{}, fmt.Errorf("no current matching provisioning profile authorizes the distribution certificate")
}

func validateDistributionProfile(profile *provisioningProfile, profileType string) error {
	if profile.ExpirationDate.IsZero() || !profile.ExpirationDate.After(time.Now().Add(5*time.Minute)) {
		return fmt.Errorf("provisioning profile is expired or near expiry")
	}
	var supportsIOS bool
	for _, platform := range profile.Platform {
		if platform == "iOS" {
			supportsIOS = true
		}
	}
	if !supportsIOS || len(profile.DeveloperCertificates) != 1 || profile.ProvisionsAllDevices {
		return fmt.Errorf("provisioning profile is not an iOS distribution profile")
	}
	// The device list is what separates the two distribution profile kinds: an
	// App Store profile names none, an ad hoc profile names exactly the devices
	// it may install onto. Accepting the wrong one here would produce an IPA
	// that silently refuses to install.
	switch profileType {
	case ascAdHocProfileType:
		if len(profile.ProvisionedDevices) == 0 {
			return fmt.Errorf("provisioning profile is not an ad hoc iOS distribution profile")
		}
	default:
		if len(profile.ProvisionedDevices) != 0 {
			return fmt.Errorf("provisioning profile is not an App Store iOS distribution profile")
		}
	}
	if getTaskAllow, ok := profile.Entitlements["get-task-allow"]; ok && getTaskAllow != false {
		return fmt.Errorf("provisioning profile allows debugging")
	}
	return nil
}

func profileAuthorizesIdentity(certificates [][]byte, identity string) bool {
	for _, certificate := range certificates {
		fingerprint := sha1.Sum(certificate) // #nosec G401 -- required to compare Apple's certificate fingerprint.
		if strings.EqualFold(hex.EncodeToString(fingerprint[:]), identity) {
			return true
		}
	}
	return false
}

func writeBase64Secret(value, destination string) error {
	data, err := decodeBase64Secret(value)
	if err != nil {
		return err
	}
	return writePrivateFile(destination, data)
}

func decodeBase64Secret(value string) ([]byte, error) {
	if len(value) > maxSecretBytes*2 {
		return nil, fmt.Errorf("secret is too large")
	}
	data, err := base64.StdEncoding.DecodeString(strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, value))
	if err != nil || len(data) == 0 || len(data) > maxSecretBytes {
		return nil, fmt.Errorf("invalid base64 secret")
	}
	return data, nil
}

func writeTextOrBase64Secret(value, destination, marker string) error {
	if strings.Contains(value, "-----BEGIN "+marker+"-----") {
		if len(value) > maxSecretBytes {
			return fmt.Errorf("secret is too large")
		}
		return writePrivateFile(destination, []byte(value+"\n"))
	}
	return writeBase64Secret(value, destination)
}

func writePrivateFile(destination string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	return os.WriteFile(destination, data, 0600)
}

func copyPrivateFile(source, destination string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return writePrivateFile(destination, data)
}

func randomPassword() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}
