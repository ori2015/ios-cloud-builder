// Package runner implements the small trusted helper used by the public
// central-builder workflow. It deliberately accepts structured iOS build
// options rather than commands or scripts.
package runner

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"filippo.io/age"
	"github.com/MobAI-App/ios-builder/internal/registry"
)

const (
	FrameworkAuto        = "auto"
	FrameworkNative      = "native"
	FrameworkFlutter     = "flutter"
	FrameworkReactNative = "react-native"
	FrameworkExpo        = "expo"
	FrameworkKMP         = "kmp"
	FrameworkCordova     = "cordova"
	FrameworkIonic       = "ionic"
)

var (
	buildIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	schemePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._+()-]{0,127}$`)
)

// Inputs are the complete set of values accepted from workflow_dispatch.
type Inputs struct {
	BuildID           string
	ProjectID         string
	ArtifactRecipient string
	Operation         string
}

// Validate rejects values that could widen repository access, escape the
// source checkout, or turn the workflow into a generic command runner.
func (in *Inputs) Validate() error {
	if !buildIDPattern.MatchString(in.BuildID) || in.BuildID == "." || in.BuildID == ".." {
		return fmt.Errorf("invalid build_id")
	}
	if !registry.ProjectIDPattern.MatchString(in.ProjectID) {
		return fmt.Errorf("invalid project_id")
	}
	recipient, err := age.ParseX25519Recipient(in.ArtifactRecipient)
	if err != nil || recipient.String() != in.ArtifactRecipient {
		return fmt.Errorf("invalid artifact_recipient")
	}
	if in.Operation != "build" && in.Operation != "testflight" {
		return fmt.Errorf("invalid operation")
	}
	return nil
}

// ResolveProject validates the secret registry, emits masks before returning
// any private metadata, and appends trusted values to the GitHub output file.
func ResolveProject(in *Inputs, registryJSON, outputPath string, commands io.Writer) error {
	if err := in.Validate(); err != nil {
		return err
	}
	value, err := registry.Parse([]byte(registryJSON))
	if err != nil {
		return fmt.Errorf("protected project registry is invalid")
	}
	project, err := value.Resolve(in.ProjectID)
	if err != nil {
		return fmt.Errorf("opaque project is not authorized")
	}
	snapshotRef := "refs/ios-builder/jobs/" + project.SnapshotNamespace + "/" + in.BuildID
	for _, value := range registry.MaskValues(&project, snapshotRef) {
		if _, err := fmt.Fprintf(commands, "::add-mask::%s\n", value); err != nil {
			return fmt.Errorf("register private metadata masks")
		}
	}
	if !filepath.IsAbs(outputPath) {
		return fmt.Errorf("invalid GitHub output path")
	}
	output, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("open GitHub output")
	}
	defer output.Close()
	configuration := project.Configuration
	if in.Operation == "testflight" {
		configuration = "Release"
	}
	values := map[string]string{
		"source_owner": project.Owner, "source_repo": project.Repo, "snapshot_ref": snapshotRef,
		"ios_path": project.IOSPath, "scheme": project.Scheme, "configuration": configuration,
		"framework_hint": project.FrameworkHint,
	}
	for _, name := range []string{"source_owner", "source_repo", "snapshot_ref", "ios_path", "scheme", "configuration", "framework_hint"} {
		if _, err := fmt.Fprintf(output, "%s=%s\n", name, values[name]); err != nil {
			return fmt.Errorf("write trusted project outputs")
		}
	}
	return output.Close()
}

func validateRelativePath(value string) error {
	if value == "" || filepath.IsAbs(value) || strings.ContainsAny(value, "\x00\r\n") || strings.Contains(value, `\`) {
		return fmt.Errorf("must be a non-empty portable relative path")
	}
	clean := filepath.Clean(value)
	if clean != value || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("must be clean and remain inside the checkout")
	}
	return nil
}

func validFramework(framework string) bool {
	switch framework {
	case FrameworkAuto, FrameworkNative, FrameworkFlutter, FrameworkReactNative,
		FrameworkExpo, FrameworkKMP, FrameworkCordova, FrameworkIonic:
		return true
	default:
		return false
	}
}
