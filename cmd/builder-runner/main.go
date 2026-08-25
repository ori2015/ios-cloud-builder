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
	case "deploy-testflight":
		return deployTestFlight(args[1:])
	default:
		return fmt.Errorf("unknown builder-runner subcommand")
	}
}

func deployTestFlight(args []string) error {
	flags := newFlags("deploy-testflight")
	var options runner.TestFlightOptions
	var recipient, outputDir string
	flags.StringVar(&options.EncryptedIPAPath, "encrypted-ipa", "", "")
	flags.StringVar(&options.LogPath, "log", "", "")
	flags.StringVar(&options.BuildNumber, "build-number", "", "")
	flags.StringVar(&recipient, "recipient", "", "")
	flags.StringVar(&outputDir, "output", "", "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return fmt.Errorf("invalid TestFlight deployment arguments")
	}
	fmt.Println("Signing and uploading iOS application; detailed output is private")
	if err := runner.ExecuteTestFlight(context.Background(), &options, recipient, outputDir); err != nil {
		if err == runner.ErrDeployFailed {
			return runner.ErrDeployFailed
		}
		return fmt.Errorf("secure TestFlight deployment preparation failed")
	}
	fmt.Println("App Store Connect accepted the upload; encrypted diagnostics are ready")
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
	flags.StringVar(&in.SourceOwner, "source-owner", "", "")
	flags.StringVar(&in.SourceRepo, "source-repo", "", "")
	flags.StringVar(&in.SnapshotRef, "snapshot-ref", "", "")
	flags.StringVar(&in.IOSPath, "ios-path", "", "")
	flags.StringVar(&in.Scheme, "scheme", "", "")
	flags.StringVar(&in.Configuration, "configuration", "", "")
	flags.StringVar(&in.FrameworkHint, "framework-hint", "", "")
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
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return fmt.Errorf("invalid secure execution arguments")
	}
	if ipaRecipient == "" {
		ipaRecipient = recipient
	}
	fmt.Println("Building iOS application; detailed output is private")
	if err := runner.ExecuteSecureWithIPARecipient(context.Background(), &options, recipient, ipaRecipient, outputDir); err != nil {
		if err == runner.ErrBuildFailed {
			return runner.ErrBuildFailed
		}
		return fmt.Errorf("secure build artifact preparation failed")
	}
	fmt.Println("Build succeeded; encrypted artifacts are ready")
	return nil
}
