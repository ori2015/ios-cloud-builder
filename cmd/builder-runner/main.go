// builder-runner is the trusted executable built from the public builder
// checkout before private source or credentials enter the job.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/MobAI-App/ios-builder/internal/runner"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("builder-runner requires a subcommand")
	}
	switch args[0] {
	case "validate-inputs":
		return validateInputs(args[1:])
	case "resolve-project":
		return resolveProject(args[1:])
	case "revoke-token":
		return revokeToken()
	case "detect":
		return detect(args[1:])
	case "verify-checkout":
		return verifyCheckout(args[1:])
	case "restore-snapshot":
		return restoreSnapshot(args[1:])
	case "execute":
		return execute(args[1:])
	case "trusted-package":
		return trustedPackage(args[1:])
	case "verify-provenance":
		return verifyProvenance(args[1:])
	case "encrypt-diagnostic":
		return encryptDiagnostic(args[1:])
	case "deploy-testflight":
		return deployTestFlight(args[1:])
	default:
		return fmt.Errorf("unknown builder-runner subcommand")
	}
}

func trustedPackage(args []string) error {
	flags := newFlags("trusted-package")
	var options runner.TrustedPackageOptions
	var recipient, logPath string
	flags.StringVar(&options.InputDir, "input", "", "")
	flags.StringVar(&options.OutputDir, "output", "", "")
	flags.StringVar(&options.BuildID, "build-id", "", "")
	flags.StringVar(&options.ProjectID, "project-id", "", "")
	flags.StringVar(&options.BuilderCommit, "builder-commit", "", "")
	flags.StringVar(&options.WorkflowRef, "workflow-ref", "", "")
	flags.StringVar(&recipient, "recipient", "", "")
	flags.StringVar(&logPath, "log", "", "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || !filepath.IsAbs(logPath) {
		return fmt.Errorf("invalid trusted packaging arguments")
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0700); err != nil {
		return fmt.Errorf("prepare private packaging log")
	}
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("prepare private packaging log")
	}
	err = runner.TrustedPackage(context.Background(), &options, recipient, logFile)
	_ = logFile.Close()
	if err != nil {
		return fmt.Errorf("trusted packaging rejected project output")
	}
	_ = os.Remove(logPath)
	fmt.Println("Trusted TestFlight intermediate and provenance are ready")
	return nil
}

func verifyProvenance(args []string) error {
	flags := newFlags("verify-provenance")
	var dir, repository, workflow, sourceRef, logPath string
	var expected runner.ProvenanceExpectation
	flags.StringVar(&dir, "input", "", "")
	flags.StringVar(&expected.BuildID, "build-id", "", "")
	flags.StringVar(&expected.ProjectID, "project-id", "", "")
	flags.StringVar(&expected.BuilderCommit, "builder-commit", "", "")
	flags.StringVar(&expected.WorkflowRef, "workflow-ref", "", "")
	flags.StringVar(&repository, "repository", "", "")
	flags.StringVar(&workflow, "signer-workflow", "", "")
	flags.StringVar(&sourceRef, "source-ref", "", "")
	flags.StringVar(&logPath, "log", "", "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || !filepath.IsAbs(logPath) {
		return fmt.Errorf("invalid provenance verification arguments")
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0700); err != nil {
		return fmt.Errorf("prepare private provenance log")
	}
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("prepare private provenance log")
	}
	defer logFile.Close()
	if _, err := runner.ValidateProvenanceArtifact(dir, expected); err != nil {
		_, _ = fmt.Fprintf(logFile, "Provenance structure rejected: %v\n", err)
		return fmt.Errorf("authenticated provenance rejected")
	}
	if err := runner.VerifyGitHubAttestations(context.Background(), dir, repository, workflow, sourceRef, expected.BuilderCommit, logFile); err != nil {
		_, _ = fmt.Fprintf(logFile, "Attestation rejected: %v\n", err)
		return fmt.Errorf("authenticated provenance rejected")
	}
	_ = os.Remove(logPath)
	fmt.Println("Authenticated provenance verified")
	return nil
}

func encryptDiagnostic(args []string) error {
	flags := newFlags("encrypt-diagnostic")
	var logPath, recipient, output string
	flags.StringVar(&logPath, "log", "", "")
	flags.StringVar(&recipient, "recipient", "", "")
	flags.StringVar(&output, "output", "", "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return fmt.Errorf("invalid diagnostic encryption arguments")
	}
	if err := runner.EncryptArtifacts(recipient, logPath, "", output); err != nil {
		return fmt.Errorf("encrypt private diagnostic")
	}
	return nil
}

func deployTestFlight(args []string) error {
	flags := newFlags("deploy-testflight")
	var options runner.TestFlightOptions
	var recipient, outputDir string
	flags.StringVar(&options.EncryptedIPAPath, "encrypted-ipa", "", "")
	flags.StringVar(&options.ManifestPath, "manifest", "", "")
	flags.StringVar(&options.LogPath, "log", "", "")
	flags.StringVar(&options.BuildNumber, "build-number", "", "")
	flags.StringVar(&options.Expected.BuildID, "build-id", "", "")
	flags.StringVar(&options.Expected.ProjectID, "project-id", "", "")
	flags.StringVar(&options.Expected.BuilderCommit, "builder-commit", "", "")
	flags.StringVar(&options.Expected.WorkflowRef, "workflow-ref", "", "")
	flags.StringVar(&recipient, "recipient", "", "")
	flags.StringVar(&outputDir, "output", "", "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return fmt.Errorf("invalid TestFlight deployment arguments")
	}
	fmt.Println("Signing and uploading iOS application; detailed output is private")
	// Bounded well under the job's own 60-minute timeout so a hung subprocess
	// (xcrun altool has no internal timeout of its own) fails with a
	// diagnosable error and a flushed, encrypted log instead of silently
	// eating the whole job budget with nothing to show for it afterward.
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	if err := runner.ExecuteTestFlight(ctx, &options, recipient, outputDir); err != nil {
		if err == runner.ErrDeployFailed {
			return runner.ErrDeployFailed
		}
		return fmt.Errorf("secure TestFlight deployment preparation failed")
	}
	fmt.Println("App Store Connect deployment completed; encrypted diagnostics are ready")
	return nil
}

func newFlags(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	return flags
}

func validateInputs(args []string) error {
	flags := newFlags("validate-inputs")
	var in runner.Inputs
	flags.StringVar(&in.BuildID, "build-id", "", "")
	flags.StringVar(&in.ProjectID, "project-id", "", "")
	flags.StringVar(&in.ArtifactRecipient, "artifact-recipient", "", "")
	flags.StringVar(&in.Operation, "operation", "", "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return fmt.Errorf("invalid validation arguments")
	}
	if err := in.Validate(); err != nil {
		return err
	}
	fmt.Println("Dispatch inputs validated")
	return nil
}

func resolveProject(args []string) error {
	flags := newFlags("resolve-project")
	var in runner.Inputs
	var registryJSON, githubOutput string
	flags.StringVar(&in.BuildID, "build-id", "", "")
	flags.StringVar(&in.ProjectID, "project-id", "", "")
	flags.StringVar(&in.ArtifactRecipient, "artifact-recipient", "", "")
	flags.StringVar(&in.Operation, "operation", "", "")
	flags.StringVar(&registryJSON, "registry", "", "")
	flags.StringVar(&githubOutput, "github-output", "", "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return fmt.Errorf("invalid project resolution arguments")
	}
	if err := runner.ResolveProject(&in, registryJSON, githubOutput, os.Stdout); err != nil {
		return err
	}
	fmt.Println("Opaque project authorization resolved")
	return nil
}

func revokeToken() error {
	token := os.Getenv("SOURCE_TOKEN")
	_ = os.Unsetenv("SOURCE_TOKEN")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := runner.RevokeInstallationToken(ctx, token); err != nil {
		return err
	}
	fmt.Println("Source credential revoked")
	return nil
}

func detect(args []string) error {
	flags := newFlags("detect")
	var source, hint string
	flags.StringVar(&source, "source", "", "")
	flags.StringVar(&hint, "framework", "", "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || !filepath.IsAbs(source) {
		return fmt.Errorf("invalid framework detection arguments")
	}
	framework, err := runner.DetectFramework(source, hint)
	if err != nil {
		return fmt.Errorf("framework detection failed")
	}
	fmt.Printf("framework=%s\n", framework)
	return nil
}

func verifyCheckout(args []string) error {
	flags := newFlags("verify-checkout")
	var source string
	flags.StringVar(&source, "source", "", "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || !filepath.IsAbs(source) {
		return fmt.Errorf("invalid checkout verification arguments")
	}
	if err := runner.VerifyCheckoutNoCredentials(source); err != nil {
		return err
	}
	fmt.Println("Private checkout contains no persisted credential")
	return nil
}

func restoreSnapshot(args []string) error {
	flags := newFlags("restore-snapshot")
	var source string
	flags.StringVar(&source, "source", "", "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || !filepath.IsAbs(source) {
		return fmt.Errorf("invalid snapshot restoration arguments")
	}
	if err := runner.RestoreLargeSnapshotFiles(source); err != nil {
		return fmt.Errorf("large snapshot restoration failed")
	}
	fmt.Println("Private snapshot transport restored")
	return nil
}

func execute(args []string) error {
	flags := newFlags("execute")
	var options runner.BuildOptions
	var recipient, ipaRecipient, outputDir string
	var projectIntermediate bool
	flags.StringVar(&options.SourceRoot, "source", "", "")
	flags.StringVar(&options.IOSPath, "ios-path", "", "")
	flags.StringVar(&options.Scheme, "scheme", "", "")
	flags.StringVar(&options.Configuration, "configuration", "", "")
	flags.StringVar(&options.Framework, "framework", "", "")
	flags.StringVar(&options.LogPath, "log", "", "")
	flags.StringVar(&options.IPAPath, "ipa", "", "")
	flags.StringVar(&recipient, "recipient", "", "")
	flags.StringVar(&ipaRecipient, "ipa-recipient", "", "")
	flags.StringVar(&outputDir, "output", "", "")
	flags.BoolVar(&projectIntermediate, "project-intermediate", false, "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return fmt.Errorf("invalid secure execution arguments")
	}
	if ipaRecipient == "" {
		ipaRecipient = recipient
	}
	fmt.Println("Building iOS application; detailed output is private")
	if err := runner.ExecuteSecureWithTransport(context.Background(), &options, recipient, ipaRecipient, outputDir, projectIntermediate); err != nil {
		if err == runner.ErrBuildFailed {
			return runner.ErrBuildFailed
		}
		return fmt.Errorf("secure build artifact preparation failed")
	}
	fmt.Println("Build succeeded; encrypted artifacts are ready")
	return nil
}
