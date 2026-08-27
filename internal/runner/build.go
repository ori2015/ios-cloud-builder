package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

var ErrBuildFailed = errors.New("private iOS build failed; download the encrypted diagnostic log")

type BuildOptions struct {
	SourceRoot    string
	IOSPath       string
	Scheme        string
	Configuration string
	Framework     string
	LogPath       string
	IPAPath       string
	// BundleID, when set, is the registered application identity this build is
	// allowed to produce. It is resolved from the protected registry before any
	// project code is checked out, and enforced below against what the build
	// actually produced.
	BundleID string
}

// ExecuteSecure keeps the trusted runner resident while private project code
// executes and while its outputs are encrypted. A project script therefore
// cannot replace the helper between the build and encryption steps.
func ExecuteSecure(ctx context.Context, options *BuildOptions, recipient, encryptedDir string) error {
	return ExecuteSecureWithIPARecipient(ctx, options, recipient, recipient, encryptedDir)
}

// ExecuteSecureWithIPARecipient encrypts the build log for the local caller
// and the unsigned IPA for either that caller or the protected signing job.
func ExecuteSecureWithIPARecipient(ctx context.Context, options *BuildOptions, logRecipient, ipaRecipient, encryptedDir string) error {
	return ExecuteSecureWithTransport(ctx, options, logRecipient, ipaRecipient, encryptedDir, false)
}

// ExecuteSecureWithTransport uses a distinct name for the untrusted encrypted
// project output handed to the isolated trusted-packaging job.
func ExecuteSecureWithTransport(ctx context.Context, options *BuildOptions, logRecipient, ipaRecipient, encryptedDir string, projectIntermediate bool) error {
	if err := options.validate(); err != nil {
		return err
	}
	if !filepath.IsAbs(encryptedDir) || filepath.Base(encryptedDir) != "encrypted" ||
		filepath.Dir(encryptedDir) != filepath.Dir(filepath.Dir(options.LogPath)) {
		return fmt.Errorf("invalid encrypted output directory")
	}
	buildErr := BuildUnsigned(ctx, options)
	if _, err := os.Lstat(options.LogPath); os.IsNotExist(err) {
		_ = os.MkdirAll(filepath.Dir(options.LogPath), 0700)
		message := []byte("The iOS build could not start. No public diagnostic details were emitted.\n")
		_ = os.WriteFile(options.LogPath, message, 0600)
	}
	ipaName := "App.ipa.age"
	if projectIntermediate {
		ipaName = "project-output.age"
	}
	if err := encryptArtifactsWithRecipientsNamed(logRecipient, ipaRecipient, options.LogPath, options.IPAPath, encryptedDir, ipaName); err != nil {
		return fmt.Errorf("encrypt private build artifacts")
	}
	if buildErr != nil {
		return ErrBuildFailed
	}
	return nil
}

// BuildUnsigned executes the supported, fixed iOS build pipeline. Every child
// process writes only to the private log, receives a scrubbed environment, and
// is invoked without a shell or eval.
func BuildUnsigned(ctx context.Context, options *BuildOptions) error {
	if err := options.validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(options.LogPath), 0700); err != nil {
		return fmt.Errorf("prepare private output")
	}
	logFile, err := os.OpenFile(options.LogPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("prepare private build log")
	}
	defer logFile.Close()
	if err := buildUnsigned(ctx, options, logFile); err != nil {
		fmt.Fprintf(logFile, "\nBuild failed: %v\n", err)
		return ErrBuildFailed
	}
	if err := logFile.Sync(); err != nil {
		return ErrBuildFailed
	}
	return nil
}

func (options *BuildOptions) validate() error {
	if !validFramework(options.Framework) || options.Framework == FrameworkAuto {
		return fmt.Errorf("invalid resolved framework")
	}
	if options.Configuration != "Debug" && options.Configuration != "Release" {
		return fmt.Errorf("invalid build configuration")
	}
	if err := validateRelativePath(options.IOSPath); err != nil {
		return fmt.Errorf("invalid iOS path")
	}
	if options.Scheme != "" && !schemePattern.MatchString(options.Scheme) {
		return fmt.Errorf("invalid scheme")
	}
	for _, path := range []string{options.SourceRoot, options.LogPath, options.IPAPath} {
		if path == "" || !filepath.IsAbs(path) {
			return fmt.Errorf("runner paths must be absolute")
		}
	}
	if filepath.Base(options.LogPath) != "build.log" || filepath.Base(options.IPAPath) != "App.ipa" ||
		filepath.Dir(options.LogPath) != filepath.Dir(options.IPAPath) ||
		filepath.Base(filepath.Dir(options.LogPath)) != "private-output" ||
		pathWithin(options.SourceRoot, filepath.Dir(options.LogPath)) {
		return fmt.Errorf("invalid private output paths")
	}
	return nil
}

type executor struct {
	ctx context.Context
	env []string
	log io.Writer
}

func (e executor) run(dir, program string, args ...string) error {
	fmt.Fprintf(e.log, "\n$ %s %s\n", program, strings.Join(args, " "))
	cmd := exec.CommandContext(e.ctx, program, args...)
	cmd.Dir = dir
	cmd.Env = e.env
	cmd.Stdout = e.log
	cmd.Stderr = e.log
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s failed: %w", filepath.Base(program), err)
	}
	return nil
}

func (e executor) capture(dir, program string, args ...string) ([]byte, error) {
	fmt.Fprintf(e.log, "\n$ %s %s\n", program, strings.Join(args, " "))
	cmd := exec.CommandContext(e.ctx, program, args...)
	cmd.Dir = dir
	cmd.Env = e.env
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = e.log
	err := cmd.Run()
	_, _ = e.log.Write(output.Bytes())
	if err != nil {
		return nil, fmt.Errorf("%s failed: %w", filepath.Base(program), err)
	}
	return output.Bytes(), nil
}

func buildUnsigned(ctx context.Context, options *BuildOptions, privateLog io.Writer) error {
	sourceRoot, err := filepath.EvalSymlinks(options.SourceRoot)
	if err != nil {
		return fmt.Errorf("resolve source checkout: %w", err)
	}
	if err := VerifyCheckoutNoCredentials(sourceRoot); err != nil {
		return err
	}
	privateHome := filepath.Join(filepath.Dir(options.LogPath), ".build-home")
	_ = os.RemoveAll(privateHome)
	if err := os.Mkdir(privateHome, 0700); err != nil {
		return fmt.Errorf("create isolated build home: %w", err)
	}
	defer func() { _ = os.RemoveAll(privateHome) }()
	env := ChildEnvironment(sourceRoot, privateHome)
	run := executor{ctx: ctx, env: env, log: privateLog}

	if isNodeFramework(options.Framework) {
		if err := installNodeDependencies(run, sourceRoot); err != nil {
			return err
		}
	}
	iosRoot := filepath.Join(sourceRoot, options.IOSPath)
	switch options.Framework {
	case FrameworkCordova:
		if err := run.run(sourceRoot, "npx", "--no-install", "cordova", "prepare", "ios"); err != nil {
			return err
		}
		iosRoot = filepath.Join(sourceRoot, "platforms", "ios")
	case FrameworkIonic:
		if isCapacitorProject(sourceRoot) {
			if err := run.run(sourceRoot, "npx", "--no-install", "cap", "sync", "ios"); err != nil {
				return err
			}
			iosRoot = filepath.Join(sourceRoot, "ios")
		} else {
			if err := run.run(sourceRoot, "npx", "--no-install", "ionic", "cordova", "prepare", "ios"); err != nil {
				return err
			}
			iosRoot = filepath.Join(sourceRoot, "platforms", "ios")
		}
	}
	iosRoot, err = filepath.EvalSymlinks(iosRoot)
	if err != nil || !pathWithin(sourceRoot, iosRoot) {
		return fmt.Errorf("iOS path is missing or escapes the private checkout")
	}

	var appPath string
	if options.Framework == FrameworkFlutter {
		if err := run.run(sourceRoot, "flutter", "pub", "get"); err != nil {
			return err
		}
		mode := "--release"
		if options.Configuration == "Debug" {
			mode = "--debug"
		}
		if err := run.run(sourceRoot, "flutter", "build", "ios", mode, "--no-codesign"); err != nil {
			return err
		}
		appPath, err = findApp(filepath.Join(sourceRoot, "build", "ios", "iphoneos"))
	} else {
		if options.Framework == FrameworkKMP {
			if err := makeGradleWrapperExecutable(sourceRoot); err != nil {
				return err
			}
		}
		workspace, project, findErr := ensureXcodeContainer(run, iosRoot)
		if findErr != nil {
			return findErr
		}
		if exists(filepath.Join(iosRoot, "Podfile")) {
			if err := run.run(iosRoot, "pod", "install"); err != nil {
				return err
			}
			// CocoaPods can create a workspace around a generated project.
			workspace, project, findErr = findXcodeContainer(iosRoot)
			if findErr != nil {
				return findErr
			}
		}
		scheme := options.Scheme
		if scheme == "" {
			scheme, err = detectScheme(run, iosRoot, workspace, project)
			if err != nil {
				return err
			}
		}
		derivedData := filepath.Join(filepath.Dir(options.LogPath), "DerivedData")
		_ = os.RemoveAll(derivedData)
		defer func() { _ = os.RemoveAll(derivedData) }()
		args := make([]string, 0, 20)
		if workspace != "" {
			args = append(args, "-workspace", workspace)
		} else {
			args = append(args, "-project", project)
		}
		args = append(args,
			"-scheme", scheme,
			"-configuration", options.Configuration,
			"-destination", "generic/platform=iOS",
			"-derivedDataPath", derivedData,
			"CODE_SIGNING_ALLOWED=NO",
			"CODE_SIGNING_REQUIRED=NO",
			"COMPILER_INDEX_STORE_ENABLE=NO",
			"SWIFT_ENABLE_COMPILE_CACHE=NO",
			"CLANG_ENABLE_COMPILE_CACHE=NO",
			"build",
		)
		if err := run.run(iosRoot, "xcodebuild", args...); err != nil {
			return err
		}
		appPath, err = findApp(filepath.Join(derivedData, "Build", "Products", options.Configuration+"-iphoneos"))
	}
	if err != nil {
		return err
	}
	appPath, err = filepath.EvalSymlinks(appPath)
	if err != nil {
		return fmt.Errorf("resolve built application: %w", err)
	}
	if !pathWithin(sourceRoot, appPath) && !pathWithin(filepath.Dir(options.LogPath), appPath) {
		return fmt.Errorf("built application escaped trusted output roots")
	}
	if err := verifyBuiltBundleID(appPath, options.BundleID); err != nil {
		return err
	}
	return packageIPA(run, appPath, options.IPAPath)
}

// verifyBuiltBundleID rejects a build whose application identity differs from
// the one pinned in the protected registry.
//
// This runs here, in the trusted runner, rather than in the signing job: the
// signing job reads the bundle identifier from this same application, and the
// IPA's hash is bound into the attested provenance, so verifying it before
// packaging means what the signer later reads has already been checked. It also
// keeps the registry out of the signing job, which must not learn which private
// repository a build came from.
func verifyBuiltBundleID(appPath, expected string) error {
	if expected == "" {
		return nil // unsigned build of a project with no pinned identity
	}
	built, _, err := readAppMetadata(filepath.Join(appPath, "Info.plist"))
	if err != nil {
		return fmt.Errorf("read built application identity: %w", err)
	}
	if built != expected {
		return fmt.Errorf("built application identity does not match the registered project")
	}
	return nil
}

func isNodeFramework(framework string) bool {
	return framework == FrameworkReactNative || framework == FrameworkExpo || framework == FrameworkCordova || framework == FrameworkIonic
}

func isCapacitorProject(root string) bool {
	for _, name := range []string{"capacitor.config.ts", "capacitor.config.js", "capacitor.config.json"} {
		if exists(filepath.Join(root, name)) {
			return true
		}
	}
	return false
}

func makeGradleWrapperExecutable(root string) error {
	path := filepath.Join(root, "gradlew")
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("invalid Gradle wrapper")
	}
	if err := os.Chmod(path, info.Mode().Perm()|0111); err != nil {
		return fmt.Errorf("make Gradle wrapper executable: %w", err)
	}
	return nil
}

func installNodeDependencies(run executor, root string) error {
	switch {
	case exists(filepath.Join(root, "pnpm-lock.yaml")):
		return run.run(root, "corepack", "pnpm", "install", "--frozen-lockfile")
	case exists(filepath.Join(root, "yarn.lock")):
		return run.run(root, "corepack", "yarn", "install", "--frozen-lockfile")
	case exists(filepath.Join(root, "package-lock.json")):
		return run.run(root, "npm", "ci")
	default:
		return run.run(root, "npm", "install")
	}
}

func findXcodeContainer(iosRoot string) (workspace, project string, err error) {
	workspaces, _ := filepath.Glob(filepath.Join(iosRoot, "*.xcworkspace"))
	projects, _ := filepath.Glob(filepath.Join(iosRoot, "*.xcodeproj"))
	sort.Strings(workspaces)
	sort.Strings(projects)
	if len(workspaces) > 0 {
		return filepath.Base(workspaces[0]), "", nil
	}
	if len(projects) > 0 {
		return "", filepath.Base(projects[0]), nil
	}
	return "", "", fmt.Errorf("no Xcode workspace or project found")
}

func ensureXcodeContainer(run executor, iosRoot string) (workspace, project string, err error) {
	workspace, project, err = findXcodeContainer(iosRoot)
	if err == nil {
		return workspace, project, nil
	}
	if !hasXcodeGenManifest(iosRoot) {
		return "", "", err
	}
	if err := run.run(iosRoot, "brew", "install", "xcodegen"); err != nil {
		return "", "", err
	}
	if err := run.run(iosRoot, "xcodegen", "generate"); err != nil {
		return "", "", err
	}
	return findXcodeContainer(iosRoot)
}

func hasXcodeGenManifest(iosRoot string) bool {
	for _, name := range []string{"project.yml", "project.yaml"} {
		contents, err := os.ReadFile(filepath.Join(iosRoot, name))
		if err != nil || len(contents) > 2*1024*1024 {
			continue
		}
		for _, line := range bytes.Split(contents, []byte{'\n'}) {
			trimmed := strings.TrimSpace(string(line))
			if trimmed == "targets:" || strings.HasPrefix(trimmed, "targets: ") {
				return true
			}
		}
	}
	return false
}

type xcodeList struct {
	Project struct {
		Schemes []string `json:"schemes"`
	} `json:"project"`
	Workspace struct {
		Schemes []string `json:"schemes"`
	} `json:"workspace"`
}

func detectScheme(run executor, iosRoot, workspace, project string) (string, error) {
	args := []string{"-list", "-json"}
	containerName := project
	if workspace != "" {
		args = append([]string{"-workspace", workspace}, args...)
		containerName = workspace
	} else {
		args = append([]string{"-project", project}, args...)
	}
	output, err := run.capture(iosRoot, "xcodebuild", args...)
	if err != nil {
		return "", err
	}
	var listing xcodeList
	if err := json.Unmarshal(output, &listing); err != nil {
		return "", fmt.Errorf("parse Xcode scheme list: %w", err)
	}
	schemes := listing.Project.Schemes
	if workspace != "" {
		schemes = listing.Workspace.Schemes
	}
	preferred := strings.TrimSuffix(strings.TrimSuffix(containerName, ".xcworkspace"), ".xcodeproj")
	for _, scheme := range schemes {
		if scheme == preferred {
			return scheme, nil
		}
	}
	for _, scheme := range schemes {
		if scheme != "" && !strings.HasPrefix(scheme, "Pods-") {
			return scheme, nil
		}
	}
	return "", fmt.Errorf("no shared application scheme found")
}

func findApp(root string) (string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", fmt.Errorf("inspect iOS build products: %w", err)
	}
	var apps []string
	for _, entry := range entries {
		if entry.IsDir() && strings.HasSuffix(entry.Name(), ".app") && !strings.HasSuffix(entry.Name(), "Tests.app") {
			apps = append(apps, filepath.Join(root, entry.Name()))
		}
	}
	sort.Strings(apps)
	if len(apps) == 0 {
		return "", fmt.Errorf("no unsigned iOS application was produced")
	}
	return apps[0], nil
}

func packageIPA(run executor, appPath, outputPath string) error {
	if err := os.MkdirAll(filepath.Dir(outputPath), 0700); err != nil {
		return fmt.Errorf("prepare IPA output")
	}
	_ = os.Remove(outputPath)
	staging, err := os.MkdirTemp(filepath.Dir(outputPath), ".ipa-package-")
	if err != nil {
		return fmt.Errorf("prepare IPA package")
	}
	defer func() { _ = os.RemoveAll(staging) }()
	payload := filepath.Join(staging, "Payload")
	if err := os.Mkdir(payload, 0700); err != nil {
		return fmt.Errorf("prepare IPA payload")
	}
	if err := run.run(staging, "/usr/bin/ditto", appPath, filepath.Join(payload, filepath.Base(appPath))); err != nil {
		return err
	}
	if err := run.run(staging, "/usr/bin/ditto", "-c", "-k", "--sequesterRsrc", "--keepParent", "Payload", outputPath); err != nil {
		return err
	}
	info, err := os.Stat(outputPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("IPA packaging produced no output")
	}
	return nil
}

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator)) &&
		!filepath.IsAbs(relative)
}
