package runner

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/MobAI-App/ios-builder/internal/registry"
	"howett.net/plist"
)

const (
	provenanceVersion = 1
	provenanceFile    = "provenance.json"
	trustedIPAFile    = "App.ipa.age"
	projectOutputFile = "project-output.age"
)

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type ProvenanceManifest struct {
	Version            int    `json:"version"`
	BuildID            string `json:"build_id"`
	ProjectID          string `json:"project_id"`
	Operation          string `json:"operation"`
	PlaintextIPASHA256 string `json:"plaintext_ipa_sha256"`
	CiphertextSHA256   string `json:"ciphertext_sha256"`
	BuilderCommit      string `json:"builder_commit"`
	WorkflowRef        string `json:"workflow_ref"`
	CreatedAt          string `json:"created_at"`
}

type ProvenanceExpectation struct {
	BuildID       string
	ProjectID     string
	BuilderCommit string
	WorkflowRef   string
}

type TrustedPackageOptions struct {
	InputDir      string
	OutputDir     string
	BuildID       string
	ProjectID     string
	BuilderCommit string
	WorkflowRef   string
}

type ipaPackager func(executor, string, string) error

func (o ProvenanceExpectation) validate() error {
	if !buildIDPattern.MatchString(o.BuildID) || !registry.ProjectIDPattern.MatchString(o.ProjectID) ||
		!regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(o.BuilderCommit) ||
		o.WorkflowRef == "" || len(o.WorkflowRef) > 512 || strings.ContainsAny(o.WorkflowRef, "\r\n\x00") {
		return errors.New("invalid expected provenance identity")
	}
	return nil
}

func TrustedPackage(ctx context.Context, options *TrustedPackageOptions, outputRecipient string, privateLog io.Writer) error {
	return trustedPackageWithPackager(ctx, options, outputRecipient, privateLog, packageIPA)
}

func trustedPackageWithPackager(ctx context.Context, options *TrustedPackageOptions, outputRecipient string, privateLog io.Writer, packager ipaPackager) error {
	if options == nil {
		return errors.New("invalid trusted packaging options")
	}
	expected := ProvenanceExpectation{
		BuildID: options.BuildID, ProjectID: options.ProjectID,
		BuilderCommit: options.BuilderCommit, WorkflowRef: options.WorkflowRef,
	}
	if err := expected.validate(); err != nil || !filepath.IsAbs(options.InputDir) || !filepath.IsAbs(options.OutputDir) {
		return errors.New("invalid trusted packaging options")
	}
	if err := requireExactRegularFiles(options.InputDir, map[string]int64{
		projectOutputFile: maxDeployIPABytes + 1024*1024,
		"build.log.age":   64 * 1024 * 1024,
	}); err != nil {
		return fmt.Errorf("reject untrusted project artifact: %w", err)
	}
	identityText := strings.TrimSpace(os.Getenv("PACKAGING_AGE_IDENTITY"))
	_ = os.Unsetenv("PACKAGING_AGE_IDENTITY")
	identity, err := age.ParseX25519Identity(identityText)
	if err != nil {
		return errors.New("protected packaging identity is invalid")
	}
	recipient, err := age.ParseX25519Recipient(outputRecipient)
	if err != nil || recipient.String() != outputRecipient {
		return errors.New("trusted output recipient is invalid")
	}
	workRoot, err := os.MkdirTemp(filepath.Dir(options.OutputDir), ".trusted-package-")
	if err != nil {
		return errors.New("prepare trusted packaging workspace")
	}
	defer func() { _ = os.RemoveAll(workRoot) }()
	untrustedIPA := filepath.Join(workRoot, "project-output.ipa")
	if err := decryptFileBounded(identity, filepath.Join(options.InputDir, projectOutputFile), untrustedIPA, maxDeployIPABytes); err != nil {
		return fmt.Errorf("decrypt project output: %w", err)
	}
	trustedRoot := filepath.Join(workRoot, "trusted-copy")
	appPath, err := extractUnsignedIPA(untrustedIPA, trustedRoot)
	_ = os.Remove(untrustedIPA)
	if err != nil {
		return fmt.Errorf("validate project application: %w", err)
	}
	if err := validateTrustedApplication(appPath); err != nil {
		return err
	}
	privateHome := filepath.Join(workRoot, "home")
	if err := os.Mkdir(privateHome, 0700); err != nil {
		return errors.New("prepare trusted packaging home")
	}
	plaintextIPA := filepath.Join(workRoot, "App.ipa")
	run := executor{ctx: ctx, env: ChildEnvironment(trustedRoot, privateHome), log: privateLog}
	if err := packager(run, appPath, plaintextIPA); err != nil {
		return fmt.Errorf("create trusted unsigned IPA: %w", err)
	}
	plainDigest, err := hashRegularFile(plaintextIPA, maxDeployIPABytes)
	if err != nil {
		return err
	}
	if err := prepareExactOutputDir(options.OutputDir); err != nil {
		return err
	}
	ciphertextPath := filepath.Join(options.OutputDir, trustedIPAFile)
	if err := encryptAndRemove(recipient, plaintextIPA, ciphertextPath, true); err != nil {
		return err
	}
	cipherDigest, err := hashRegularFile(ciphertextPath, maxDeployIPABytes+1024*1024)
	if err != nil {
		return err
	}
	manifest, err := newProvenanceManifest(options, plainDigest, cipherDigest, time.Now())
	if err != nil {
		return err
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return errors.New("create provenance manifest")
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(options.OutputDir, provenanceFile), data, 0600); err != nil {
		return errors.New("write provenance manifest")
	}
	return nil
}

func validateTrustedApplication(appPath string) error {
	if err := rejectNestedApplications(appPath); err != nil {
		return err
	}
	infoPath := filepath.Join(appPath, "Info.plist")
	if _, _, err := readAppMetadata(infoPath); err != nil {
		return fmt.Errorf("validate application metadata: %w", err)
	}
	data, err := os.ReadFile(infoPath)
	if err != nil || len(data) > 4*1024*1024 {
		return errors.New("validate application executable metadata")
	}
	var info struct {
		Executable string `plist:"CFBundleExecutable"`
	}
	if _, err := plist.Unmarshal(data, &info); err != nil || info.Executable == "" || filepath.Base(info.Executable) != info.Executable ||
		strings.ContainsAny(info.Executable, `/\`+"\r\n\x00") {
		return errors.New("validate application executable metadata")
	}
	executable, err := os.Lstat(filepath.Join(appPath, info.Executable))
	if err != nil || !executable.Mode().IsRegular() || executable.Mode()&os.ModeSymlink != 0 || executable.Size() == 0 {
		return errors.New("validate application executable")
	}
	return nil
}

func newProvenanceManifest(options *TrustedPackageOptions, plainDigest, cipherDigest string, createdAt time.Time) (*ProvenanceManifest, error) {
	if options == nil || !sha256Pattern.MatchString(plainDigest) || !sha256Pattern.MatchString(cipherDigest) {
		return nil, errors.New("invalid provenance manifest inputs")
	}
	expected := ProvenanceExpectation{
		BuildID: options.BuildID, ProjectID: options.ProjectID,
		BuilderCommit: options.BuilderCommit, WorkflowRef: options.WorkflowRef,
	}
	if err := expected.validate(); err != nil || createdAt.IsZero() {
		return nil, errors.New("invalid provenance manifest inputs")
	}
	return &ProvenanceManifest{
		Version: provenanceVersion, BuildID: options.BuildID, ProjectID: options.ProjectID, Operation: "testflight",
		PlaintextIPASHA256: plainDigest, CiphertextSHA256: cipherDigest, BuilderCommit: options.BuilderCommit,
		WorkflowRef: options.WorkflowRef, CreatedAt: createdAt.UTC().Format(time.RFC3339),
	}, nil
}

func ValidateProvenanceArtifact(dir string, expected ProvenanceExpectation) (*ProvenanceManifest, error) {
	if err := expected.validate(); err != nil {
		return nil, err
	}
	if err := requireExactRegularFiles(dir, map[string]int64{
		trustedIPAFile: maxDeployIPABytes + 1024*1024,
		provenanceFile: 16 * 1024,
	}); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, provenanceFile))
	if err != nil {
		return nil, errors.New("read provenance manifest")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var manifest ProvenanceManifest
	if err := decoder.Decode(&manifest); err != nil {
		return nil, errors.New("invalid provenance manifest")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid provenance manifest")
	}
	createdAt, err := time.Parse(time.RFC3339, manifest.CreatedAt)
	if err != nil || createdAt.After(time.Now().Add(10*time.Minute)) || createdAt.Before(time.Now().Add(-24*time.Hour)) {
		return nil, errors.New("invalid provenance timestamp")
	}
	if manifest.Version != provenanceVersion || manifest.BuildID != expected.BuildID || manifest.ProjectID != expected.ProjectID ||
		manifest.Operation != "testflight" || manifest.BuilderCommit != expected.BuilderCommit || manifest.WorkflowRef != expected.WorkflowRef ||
		!sha256Pattern.MatchString(manifest.PlaintextIPASHA256) || !sha256Pattern.MatchString(manifest.CiphertextSHA256) {
		return nil, errors.New("provenance identity mismatch")
	}
	digest, err := hashRegularFile(filepath.Join(dir, trustedIPAFile), maxDeployIPABytes+1024*1024)
	if err != nil || subtle.ConstantTimeCompare([]byte(digest), []byte(manifest.CiphertextSHA256)) != 1 {
		return nil, errors.New("provenance ciphertext digest mismatch")
	}
	return &manifest, nil
}

func VerifyGitHubAttestations(ctx context.Context, dir, repository, signerWorkflow, sourceRef, commit string, privateLog io.Writer) error {
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(repository) ||
		!strings.HasPrefix(signerWorkflow, repository+"/.github/workflows/") ||
		!regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(commit) || !strings.HasPrefix(sourceRef, "refs/heads/") {
		return errors.New("invalid attestation policy")
	}
	gh, err := exec.LookPath("gh")
	if err != nil {
		return errors.New("GitHub CLI is unavailable")
	}
	for _, name := range []string{trustedIPAFile, provenanceFile} {
		args := []string{"attestation", "verify", filepath.Join(dir, name), "--repo", repository,
			"--signer-workflow", signerWorkflow, "--signer-digest", commit, "--source-digest", commit,
			"--source-ref", sourceRef, "--deny-self-hosted-runners", "--format", "json"}
		cmd := exec.CommandContext(ctx, gh, args...)
		cmd.Env = attestationEnvironment()
		cmd.Stdout, cmd.Stderr = privateLog, privateLog
		if err := cmd.Run(); err != nil {
			return errors.New("GitHub provenance attestation rejected")
		}
	}
	return nil
}

func attestationEnvironment() []string {
	allowed := map[string]bool{"PATH": true, "HOME": true, "GH_TOKEN": true, "GITHUB_TOKEN": true, "RUNNER_TEMP": true}
	var result []string
	for _, entry := range os.Environ() {
		name, _, ok := strings.Cut(entry, "=")
		if ok && allowed[name] {
			result = append(result, entry)
		}
	}
	return result
}

func requireExactRegularFiles(dir string, expected map[string]int64) error {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("artifact root is invalid")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != len(expected) {
		return errors.New("artifact file allowlist mismatch")
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
		limit, ok := expected[entry.Name()]
		entryInfo, statErr := entry.Info()
		if !ok || statErr != nil || !entryInfo.Mode().IsRegular() || entryInfo.Mode()&os.ModeSymlink != 0 || entryInfo.Size() <= 0 || entryInfo.Size() > limit {
			return errors.New("artifact contains an unexpected member")
		}
	}
	sort.Strings(names)
	return nil
}

func prepareExactOutputDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return errors.New("create trusted output")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("trusted output root is invalid")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		return errors.New("trusted output root is not empty")
	}
	return os.Chmod(dir, 0700)
}

func hashRegularFile(path string, limit int64) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > limit {
		return "", errors.New("invalid provenance file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("open provenance file")
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, limit+1)); err != nil {
		return "", errors.New("hash provenance file")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func verifyPlaintextIPADigest(path string, manifest *ProvenanceManifest) error {
	if manifest == nil || !sha256Pattern.MatchString(manifest.PlaintextIPASHA256) {
		return errors.New("invalid plaintext provenance")
	}
	digest, err := hashRegularFile(path, maxDeployIPABytes)
	if err != nil || subtle.ConstantTimeCompare([]byte(digest), []byte(manifest.PlaintextIPASHA256)) != 1 {
		return errors.New("plaintext IPA digest mismatch")
	}
	return nil
}
