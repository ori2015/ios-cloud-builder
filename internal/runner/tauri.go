package runner

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/MobAI-App/ios-builder/internal/ipacheck"
)

// buildTauri builds a Tauri 2 iOS app without code signing and leaves the IPA at
// options.IPAPath. Unlike the Xcode-based frameworks, `tauri ios build --no-sign`
// itself archives and zips the unsigned application (Tauri CLI: "Skips code
// signing... the IPA is created by zipping the .app from the archive directly"),
// so there is no separate xcodebuild step to run here.
func buildTauri(run executor, appRoot string, options *BuildOptions) error {
	program, prefix, err := tauriCommand(run, appRoot)
	if err != nil {
		return err
	}
	cli := func(args ...string) error {
		return run.run(appRoot, program, append(append([]string{}, prefix...), args...)...)
	}
	if err := run.run(appRoot, "rustup", "target", "add", "aarch64-apple-ios"); err != nil {
		return err
	}
	appleDir := filepath.Join(appRoot, "src-tauri", "gen", "apple")
	if !exists(appleDir) {
		if err := cli("ios", "init", "--ci"); err != nil {
			return err
		}
	}
	build := []string{"ios", "build", "--ci", "--no-sign"}
	if options.Configuration == "Debug" {
		build = append(build, "--debug")
	}
	if err := cli(build...); err != nil {
		return err
	}
	ipa, err := findTauriIPA(appleDir)
	if err != nil {
		return err
	}
	return finishPrebuiltIPA(ipa, options)
}

// tauriCommand picks the Tauri CLI: the project's own @tauri-apps/cli when it
// depends on it, else cargo-tauri (installed if missing).
func tauriCommand(run executor, appRoot string) (program string, prefix []string, err error) {
	if pkg, readErr := os.ReadFile(filepath.Join(appRoot, "package.json")); readErr == nil && strings.Contains(string(pkg), `"@tauri-apps/cli"`) {
		return "npx", []string{"--no-install", "tauri"}, nil
	}
	if _, probe := run.capture(appRoot, "cargo", "tauri", "--version"); probe != nil {
		if err := run.run(appRoot, "cargo", "install", "tauri-cli", "--version", "^2", "--locked"); err != nil {
			return "", nil, err
		}
	}
	return "cargo", []string{"tauri"}, nil
}

func findTauriIPA(appleDir string) (string, error) {
	var found []string
	_ = filepath.WalkDir(filepath.Join(appleDir, "build"), func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && strings.HasSuffix(entry.Name(), ".ipa") {
			found = append(found, path)
		}
		return nil
	})
	sort.Strings(found)
	if len(found) == 0 {
		return "", fmt.Errorf("tauri ios build finished but left no .ipa under src-tauri/gen/apple/build; check the log above for the archive step")
	}
	return found[0], nil
}

// finishPrebuiltIPA copies an IPA a framework tool produced to the private
// output path and applies the same identity check as the Xcode-based pipeline.
func finishPrebuiltIPA(source string, options *BuildOptions) error {
	info, err := ipacheck.Inspect(source)
	if err != nil {
		return fmt.Errorf("the built IPA is not valid: %w", err)
	}
	if options.BundleID != "" && info.BundleID != options.BundleID {
		return fmt.Errorf("built application identity does not match the registered project")
	}
	if err := os.MkdirAll(filepath.Dir(options.IPAPath), 0o700); err != nil {
		return fmt.Errorf("prepare IPA output")
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(options.IPAPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
