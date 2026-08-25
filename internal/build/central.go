package build

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/MobAI-App/ios-builder/internal/config"
	"github.com/MobAI-App/ios-builder/internal/github"
	"github.com/MobAI-App/ios-builder/internal/security"
	"github.com/MobAI-App/ios-builder/internal/snapshot"
	"github.com/google/uuid"
	"howett.net/plist"
)

const (
	centralArtifactPrefix = "ios-builder-"
	centralDeployPrefix   = "ios-builder-deploy-"
	centralTrustedPrefix  = "ios-builder-trusted-"
	centralIPAFile        = "App.ipa.age"
	centralLogFile        = "build.log.age"
	centralProjectFile    = "project-output.age"

	maxArtifactArchiveSize = int64(1024*1024*1024 + 80*1024*1024)
	maxIPACiphertextSize   = int64(1024 * 1024 * 1024)
	maxLogCiphertextSize   = int64(64 * 1024 * 1024)
	maxIPAEntries          = 100000
	maxInfoPlistSize       = int64(4 * 1024 * 1024)
	cleanupTimeout         = 30 * time.Second
	cancelWaitTimeout      = 20 * time.Second
	artifactIndexTimeout   = 30 * time.Second
)

type encryptedArtifact struct {
	ipa []byte
	log []byte
}

func (c *Coordinator) buildCentral(parent context.Context, opts BuildOptions) (*BuildResult, error) {
	started := time.Now()
	if opts.Timeout == 0 {
		opts.Timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(parent, opts.Timeout)
	defer cancel()

	buildID := uuid.NewString()
	result := &BuildResult{BuildID: buildID, TestFlight: opts.TestFlight}
	c.progress.Start(buildID)

	if err := c.config.Validate(); err != nil {
		return result, err
	}
	identity, err := centralIdentity(c.config)
	if err != nil {
		return result, err
	}
	if err := snapshot.VerifyRemote(ctx, opts.Remote, c.config.GitHub.Owner, c.config.GitHub.Repo); err != nil {
		return result, fmt.Errorf("verify private source remote: %w", err)
	}

	c.progress.Update(PhaseSnapshot, "Snapshotting working tree...")
	sha, err := snapshot.Create(ctx, fmt.Sprintf("ios-builder snapshot %s", buildID))
	if err != nil {
		c.progress.Error(PhaseSnapshot, err)
		return result, fmt.Errorf("failed to snapshot working tree: %w", err)
	}
	ref := snapshot.RefForNamespace(c.config.SnapshotNamespace, buildID)
	if err := snapshot.Push(ctx, opts.Remote, sha, ref); err != nil {
		c.progress.Error(PhaseSnapshot, err)
		return result, fmt.Errorf("failed to push private snapshot: %w", err)
	}
	defer c.deleteSnapshotLease(opts.Remote, ref, sha)
	c.progress.Complete(PhaseSnapshot, fmt.Sprintf("Pushed %s", sha[:7]))

	owner, repo, workflow := c.config.Builder.Owner, c.config.Builder.Repo, c.config.Builder.Workflow
	c.progress.Update(PhaseTriggering, "Triggering central GitHub Actions build...")
	if err := c.github.TriggerWorkflow(ctx, owner, repo, workflow, centralDispatchInputs(c.config, buildID, opts.TestFlight)); err != nil {
		c.progress.Error(PhaseTriggering, err)
		return result, fmt.Errorf("failed to trigger central workflow: %w", err)
	}
	c.progress.Complete(PhaseTriggering, "Workflow triggered")
	var runID int64
	runCompleted := false
	defer func() {
		if runCompleted {
			return
		}
		if runID != 0 {
			c.cancelCentralRun(owner, repo, runID, buildID)
			return
		}
		c.cancelCentralRunByBuildID(owner, repo, workflow, buildID)
	}()

	c.progress.Update(PhaseWaitingStart, "Waiting for workflow to start...")
	run, err := c.github.PollForWorkflowStart(ctx, owner, repo, workflow, buildID, 2*time.Minute)
	if err != nil {
		c.progress.Error(PhaseWaitingStart, err)
		return result, fmt.Errorf("central workflow failed to start: %w", err)
	}
	runID = run.ID
	result.WorkflowURL = run.HTMLURL
	c.progress.SetWorkflowURL(run.HTMLURL)
	c.progress.Complete(PhaseWaitingStart, fmt.Sprintf("Workflow started (run #%d)", run.ID))

	c.progress.Update(PhaseBuilding, "Building securely on the central runner...")
	run, err = c.github.PollForWorkflowCompletion(ctx, owner, repo, run.ID, opts.Timeout, func() {
		c.showCentralRunningStep(ctx, owner, repo, run.ID)
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && opts.TestFlight {
			err = fmt.Errorf("TestFlight deployment exceeded its %s deadline; if sign-and-deploy is waiting, approve the apple-production Environment before retrying: %w", opts.Timeout, err)
		}
		c.progress.Error(PhaseBuilding, err)
		return result, fmt.Errorf("central build did not complete: %w", err)
	}
	runCompleted = true

	baseArtifact, err := c.github.PollForRunArtifact(ctx, owner, repo, run.ID, centralArtifactPrefix+buildID, artifactIndexTimeout)
	if err != nil {
		c.progress.Error(PhaseBuilding, err)
		return result, fmt.Errorf("encrypted build artifact unavailable: %w", err)
	}
	defer c.deleteCentralArtifact(owner, repo, baseArtifact.ID)
	if opts.TestFlight {
		if trustedArtifact, findErr := c.github.FindArtifactByName(ctx, owner, repo, run.ID, centralTrustedPrefix+buildID); findErr == nil {
			defer c.deleteCentralArtifact(owner, repo, trustedArtifact.ID)
		}
	}
	artifact := baseArtifact
	if opts.TestFlight {
		deployName := centralDeployPrefix + buildID
		if run.Conclusion == "success" {
			artifact, err = c.github.PollForRunArtifact(ctx, owner, repo, run.ID, deployName, artifactIndexTimeout)
			if err != nil {
				return result, fmt.Errorf("encrypted TestFlight diagnostic unavailable: %w", err)
			}
		} else if deployArtifact, findErr := c.github.FindArtifactByName(ctx, owner, repo, run.ID, deployName); findErr == nil {
			artifact = deployArtifact
		}
		if artifact.ID != baseArtifact.ID {
			defer c.deleteCentralArtifact(owner, repo, artifact.ID)
		}
	}

	c.progress.Update(PhaseDownloading, "Downloading encrypted build artifact...")
	contents, err := c.downloadCentralArtifact(ctx, owner, repo, artifact)
	if err != nil {
		c.progress.Error(PhaseDownloading, err)
		return result, err
	}

	if run.Conclusion != "success" {
		logPath, logErr := decryptLogToFile(identity, contents.log, opts.OutputDir, buildID)
		if logErr != nil {
			c.progress.Error(PhaseBuilding, logErr)
			return result, fmt.Errorf("central workflow concluded %s; decrypt diagnostics: %w", run.Conclusion, logErr)
		}
		result.LogPath = logPath
		result.Duration = time.Since(started)
		c.progress.Error(PhaseBuilding, fmt.Errorf("workflow concluded %s", run.Conclusion))
		kind := "build"
		if opts.TestFlight {
			kind = "TestFlight deployment"
		}
		return result, fmt.Errorf("%s failed with conclusion %s; decrypted diagnostics: %s", kind, run.Conclusion, logPath)
	}
	if opts.TestFlight {
		result.Duration = time.Since(started)
		c.progress.Complete(PhaseBuilding, "Signed TestFlight deployment completed")
		c.progress.Finish()
		return result, nil
	}
	if len(contents.ipa) == 0 {
		return result, errors.New("successful central artifact is missing App.ipa.age")
	}
	plaintextIPA, err := decryptBounded(identity, contents.ipa, maxIPACiphertextSize)
	if err != nil {
		return result, fmt.Errorf("decrypt IPA: %w", err)
	}
	if err := validateIPA(plaintextIPA); err != nil {
		return result, fmt.Errorf("validate decrypted IPA: %w", err)
	}
	ipaPath := filepath.Join(opts.OutputDir, centralOutputName(c.config.Project, "", ".ipa"))
	if err := atomicWritePrivate(ipaPath, plaintextIPA); err != nil {
		return result, fmt.Errorf("save decrypted IPA: %w", err)
	}

	result.IPAPath = ipaPath
	result.IPASize = int64(len(plaintextIPA))
	result.Duration = time.Since(started)
	c.progress.Complete(PhaseBuilding, "Build completed successfully")
	c.progress.Complete(PhaseDownloading, fmt.Sprintf("IPA decrypted (%.2f MB)", float64(result.IPASize)/(1024*1024)))
	c.progress.Finish()
	return result, nil
}

// DownloadLogs retrieves and decrypts the diagnostics for an exact central
// build ID. It is used by `builder ios logs <build-id>` and deliberately does
// not support repository-backend artifacts, which are plaintext upstream.
func (c *Coordinator) DownloadLogs(ctx context.Context, buildID, outputDir string) (string, error) {
	if !c.config.IsCentral() {
		return "", errors.New("encrypted build logs are available only for the central backend")
	}
	if err := c.config.Validate(); err != nil {
		return "", err
	}
	parsed, err := uuid.Parse(buildID)
	if err != nil || parsed.String() != buildID || parsed.Version() != 4 {
		return "", errors.New("build ID must be a canonical lowercase UUIDv4")
	}
	identity, err := centralIdentity(c.config)
	if err != nil {
		return "", err
	}
	owner, repo, workflow := c.config.Builder.Owner, c.config.Builder.Repo, c.config.Builder.Workflow
	run, err := c.github.FindWorkflowRunByBuildID(ctx, owner, repo, workflow, buildID)
	if err != nil {
		return "", err
	}
	artifact, err := c.github.FindArtifactByName(ctx, owner, repo, run.ID, centralDeployPrefix+buildID)
	if err != nil {
		artifact, err = c.github.FindArtifactByName(ctx, owner, repo, run.ID, centralArtifactPrefix+buildID)
	}
	if err != nil {
		return "", err
	}
	contents, err := c.downloadCentralArtifact(ctx, owner, repo, artifact)
	if err != nil {
		return "", err
	}
	path, err := decryptLogToFile(identity, contents.log, outputDir, buildID)
	if err != nil {
		return "", err
	}
	c.deleteCentralArtifact(owner, repo, artifact.ID)
	return path, nil
}

func centralIdentity(cfg *config.Config) (*age.X25519Identity, error) {
	store, err := security.NewIdentityStore()
	if err != nil {
		return nil, fmt.Errorf("open local AGE identity store: %w", err)
	}
	identity, err := store.Identity()
	if err != nil {
		return nil, fmt.Errorf("load local AGE identity (run `builder security init` first): %w", err)
	}
	if identity.Recipient().String() != strings.TrimSpace(cfg.Security.Recipient) {
		return nil, errors.New("local AGE identity does not match security.recipient in builder.json")
	}
	return identity, nil
}

func centralDispatchInputs(cfg *config.Config, buildID string, testFlight bool) map[string]string {
	inputs := map[string]string{
		"build_id":           buildID,
		"project_id":         cfg.ProjectID,
		"artifact_recipient": strings.TrimSpace(cfg.Security.Recipient),
		"operation":          "build",
	}
	if testFlight {
		inputs["operation"] = "testflight"
	}
	return inputs
}

func (c *Coordinator) downloadCentralArtifact(ctx context.Context, owner, repo string, artifact *github.Artifact) (*encryptedArtifact, error) {
	if artifact.SizeInBytes > maxArtifactArchiveSize {
		return nil, fmt.Errorf("encrypted artifact metadata exceeds %d byte limit", maxArtifactArchiveSize)
	}
	data, err := c.github.DownloadArtifactWithProgressLimit(ctx, owner, repo, artifact.ID, maxArtifactArchiveSize, func(downloaded, total int64) {
		c.progress.UpdateDownloadProgress(downloaded, total)
	})
	if err != nil {
		return nil, fmt.Errorf("download encrypted artifact: %w", err)
	}
	if err := verifyArtifactDigest(artifact.Digest, data); err != nil {
		return nil, err
	}
	return parseEncryptedArtifact(data)
}

func verifyArtifactDigest(digest string, data []byte) error {
	if digest == "" {
		return nil
	}
	algorithm, encoded, ok := strings.Cut(digest, ":")
	if !ok || algorithm != "sha256" || len(encoded) != sha256.Size*2 {
		return fmt.Errorf("artifact API returned malformed SHA-256 digest")
	}
	expected, err := hex.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("artifact API returned malformed SHA-256 digest")
	}
	actual := sha256.Sum256(data)
	if !bytes.Equal(expected, actual[:]) {
		return errors.New("downloaded artifact SHA-256 digest does not match GitHub metadata")
	}
	return nil
}

func parseEncryptedArtifact(data []byte) (*encryptedArtifact, error) {
	if int64(len(data)) > maxArtifactArchiveSize {
		return nil, fmt.Errorf("artifact archive exceeds %d byte limit", maxArtifactArchiveSize)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("open artifact ZIP: %w", err)
	}
	if len(zr.File) == 0 || len(zr.File) > 2 {
		return nil, fmt.Errorf("artifact ZIP must contain one or two ciphertext files, got %d", len(zr.File))
	}
	result := &encryptedArtifact{}
	for _, file := range zr.File {
		if file.FileInfo().IsDir() {
			return nil, fmt.Errorf("unexpected directory %q in artifact ZIP", file.Name)
		}
		switch file.Name {
		case centralIPAFile:
			if result.ipa != nil {
				return nil, fmt.Errorf("duplicate %s in artifact ZIP", centralIPAFile)
			}
			result.ipa, err = readBoundedZipFile(file, maxIPACiphertextSize)
		case centralLogFile:
			if result.log != nil {
				return nil, fmt.Errorf("duplicate %s in artifact ZIP", centralLogFile)
			}
			result.log, err = readBoundedZipFile(file, maxLogCiphertextSize)
		case centralProjectFile:
			_, err = readBoundedZipFile(file, maxIPACiphertextSize)
		default:
			return nil, fmt.Errorf("unexpected artifact ZIP member %q", file.Name)
		}
		if err != nil {
			return nil, err
		}
	}
	if len(result.log) == 0 {
		return nil, fmt.Errorf("artifact ZIP is missing %s", centralLogFile)
	}
	return result, nil
}

func readBoundedZipFile(file *zip.File, limit int64) ([]byte, error) {
	if !file.Mode().IsRegular() {
		return nil, fmt.Errorf("artifact member %q is not a regular file", file.Name)
	}
	if file.UncompressedSize64 > uint64(limit) {
		return nil, fmt.Errorf("artifact member %q exceeds %d byte limit", file.Name, limit)
	}
	r, err := file.Open()
	if err != nil {
		return nil, fmt.Errorf("open artifact member %q: %w", file.Name, err)
	}
	defer r.Close()
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read artifact member %q: %w", file.Name, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("artifact member %q exceeds %d byte limit", file.Name, limit)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("artifact member %q is empty", file.Name)
	}
	return data, nil
}

func validateIPA(data []byte) error {
	if int64(len(data)) > maxIPACiphertextSize {
		return fmt.Errorf("IPA exceeds %d byte limit", maxIPACiphertextSize)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("open IPA ZIP: %w", err)
	}
	if len(zr.File) == 0 || len(zr.File) > maxIPAEntries {
		return fmt.Errorf("invalid IPA ZIP entry count %d", len(zr.File))
	}
	apps := make(map[string]bool)
	files := make(map[string]*zip.File, len(zr.File))
	for _, file := range zr.File {
		name := strings.ReplaceAll(file.Name, "\\", "/")
		cleanName := strings.TrimSuffix(name, "/")
		if name != file.Name || strings.HasPrefix(name, "/") || cleanName == "" || path.Clean(cleanName) != cleanName {
			return fmt.Errorf("unsafe IPA ZIP member %q", file.Name)
		}
		if _, exists := files[name]; exists {
			return fmt.Errorf("duplicate IPA ZIP member %q", file.Name)
		}
		files[name] = file
		if strings.HasPrefix(name, "Payload/") {
			rest := strings.TrimPrefix(name, "Payload/")
			part := strings.SplitN(rest, "/", 2)[0]
			if strings.HasSuffix(part, ".app") && part != ".app" {
				apps["Payload/"+part] = true
			}
		}
	}
	if len(apps) != 1 {
		return fmt.Errorf("IPA must contain exactly one Payload/*.app, got %d", len(apps))
	}
	var appPath string
	for path := range apps {
		appPath = path
	}
	infoFile := files[appPath+"/Info.plist"]
	if infoFile == nil || infoFile.FileInfo().IsDir() {
		return errors.New("IPA app is missing Info.plist")
	}
	infoData, err := readBoundedZipFile(infoFile, maxInfoPlistSize)
	if err != nil {
		return err
	}
	var info struct {
		Executable string `plist:"CFBundleExecutable"`
	}
	if _, err := plist.Unmarshal(infoData, &info); err != nil {
		return fmt.Errorf("parse app Info.plist: %w", err)
	}
	if info.Executable == "" || filepath.Base(info.Executable) != info.Executable {
		return errors.New("invalid CFBundleExecutable in Info.plist")
	}
	executable := files[appPath+"/"+info.Executable]
	if executable == nil || !executable.Mode().IsRegular() || executable.UncompressedSize64 == 0 {
		return fmt.Errorf("IPA app executable %q is missing or empty", info.Executable)
	}
	return nil
}

func decryptLogToFile(identity age.Identity, ciphertext []byte, outputDir, buildID string) (string, error) {
	if len(ciphertext) == 0 {
		return "", errors.New("encrypted diagnostic log is missing")
	}
	plaintext, err := decryptBounded(identity, ciphertext, maxLogCiphertextSize)
	if err != nil {
		return "", fmt.Errorf("decrypt build log: %w", err)
	}
	path := filepath.Join(outputDir, centralOutputName("ios-builder", buildID, ".log"))
	if err := atomicWritePrivate(path, plaintext); err != nil {
		return "", fmt.Errorf("save decrypted build log: %w", err)
	}
	return path, nil
}

func decryptBounded(identity age.Identity, ciphertext []byte, limit int64) ([]byte, error) {
	reader, err := age.Decrypt(bytes.NewReader(ciphertext), identity)
	if err != nil {
		return nil, fmt.Errorf("initialize AGE decryption: %w", err)
	}
	plaintext, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, fmt.Errorf("decrypt data: %w", err)
	}
	if int64(len(plaintext)) > limit {
		return nil, fmt.Errorf("decrypted data exceeds %d byte limit", limit)
	}
	return plaintext, nil
}

func centralOutputName(project, buildID, extension string) string {
	project = strings.TrimSpace(project)
	project = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '-'
	}, project)
	project = strings.Trim(project, ".-")
	if project == "" {
		project = "App"
	}
	if buildID == "" {
		return project + extension
	}
	return project + "-" + buildID + extension
}

func atomicWritePrivate(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ios-builder-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func (c *Coordinator) showCentralRunningStep(ctx context.Context, owner, repo string, runID int64) {
	step, total, err := c.github.RunningStep(ctx, owner, repo, runID)
	if err == nil && step != nil {
		c.progress.UpdateStep(step.Name, step.Number, total, time.Since(step.StartedAt))
	}
}

func (c *Coordinator) deleteSnapshotLease(remote, ref, sha string) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	if err := snapshot.DeleteLease(ctx, remote, ref, sha); err != nil {
		c.progress.Warn(fmt.Sprintf("could not delete snapshot ref %s: %v", ref, err))
	}
}

func (c *Coordinator) cancelCentralRun(owner, repo string, runID int64, buildID string) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	if err := c.github.CancelWorkflowRun(ctx, owner, repo, runID); err != nil {
		c.progress.Warn(fmt.Sprintf("could not cancel workflow run %d: %v", runID, err))
	}
	if _, err := c.github.PollForWorkflowCompletion(ctx, owner, repo, runID, cancelWaitTimeout, nil); err != nil {
		c.progress.Warn(fmt.Sprintf("could not confirm workflow run %d stopped before artifact cleanup: %v", runID, err))
	}
	c.deleteCentralBuildArtifacts(ctx, owner, repo, runID, buildID)
}

func (c *Coordinator) cancelCentralRunByBuildID(owner, repo, workflow, buildID string) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	run, err := c.github.PollForWorkflowStart(ctx, owner, repo, workflow, buildID, cleanupTimeout)
	if err != nil {
		c.progress.Warn(fmt.Sprintf("could not locate dispatched workflow %s for cancellation: %v", buildID, err))
		return
	}
	if err := c.github.CancelWorkflowRun(ctx, owner, repo, run.ID); err != nil {
		c.progress.Warn(fmt.Sprintf("could not cancel workflow run %d: %v", run.ID, err))
	}
	if _, err := c.github.PollForWorkflowCompletion(ctx, owner, repo, run.ID, cancelWaitTimeout, nil); err != nil {
		c.progress.Warn(fmt.Sprintf("could not confirm workflow run %d stopped before artifact cleanup: %v", run.ID, err))
	}
	c.deleteCentralBuildArtifacts(ctx, owner, repo, run.ID, buildID)
}

func (c *Coordinator) deleteCentralBuildArtifacts(ctx context.Context, owner, repo string, runID int64, buildID string) {
	if err := c.github.DeleteRunArtifactsByName(ctx, owner, repo, runID,
		centralArtifactPrefix+buildID, centralTrustedPrefix+buildID, centralDeployPrefix+buildID); err != nil {
		c.progress.Warn(fmt.Sprintf("could not delete encrypted artifacts for workflow run %d: %v", runID, err))
	}
}

func (c *Coordinator) deleteCentralArtifact(owner, repo string, artifactID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	if err := c.github.DeleteArtifact(ctx, owner, repo, artifactID); err != nil {
		c.progress.Warn(fmt.Sprintf("could not delete encrypted artifact %d: %v", artifactID, err))
	}
}
