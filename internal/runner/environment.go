package runner

import (
	"os"
	"path/filepath"
	"strings"
)

// ChildEnvironment intentionally uses an allowlist and an isolated HOME. In particular, no
// GITHUB_*, ACTIONS_*, RUNNER_*, token, credential, or AGE values cross into
// dependency installers, build phases, Gradle, Node, or project scripts.
func ChildEnvironment(sourceRoot, privateHome string) []string {
	allowed := map[string]bool{
		"PATH": true, "TMPDIR": true, "TMP": true, "TEMP": true,
		"LANG": true, "LC_ALL": true, "LC_CTYPE": true, "SHELL": true,
		"USER": true, "LOGNAME": true, "DEVELOPER_DIR": true, "SDKROOT": true,
		"JAVA_HOME": true, "FLUTTER_ROOT": true, "PUB_CACHE": true,
		"GEM_HOME": true, "GEM_PATH": true, "COCOAPODS_HOME": true,
		"NODE_PATH": true, "NVM_DIR": true,
		"RUSTUP_HOME": true,
	}
	env := make([]string, 0, len(allowed)+8)
	for _, entry := range os.Environ() {
		name, _, ok := strings.Cut(entry, "=")
		if ok && allowed[name] {
			env = append(env, entry)
		}
	}
	env = withRustToolchain(env, privateHome)
	return append(env,
		"HOME="+privateHome,
		"CI=true",
		"CODE_SIGNING_ALLOWED=NO",
		"CODE_SIGNING_REQUIRED=NO",
		"COMPILER_INDEX_STORE_ENABLE=NO",
		"SWIFT_ENABLE_COMPILE_CACHE=NO",
		"CLANG_ENABLE_COMPILE_CACHE=NO",
		"PROJECT_DIR="+sourceRoot,
	)
}

// withRustToolchain lets the isolated build find the runner's Rust toolchains
// without sharing its cargo state. rustup resolves toolchains under
// RUSTUP_HOME, which defaults to the real user's ~/.rustup and would be empty
// under the isolated HOME, so it is pointed at the real directory when that
// exists. CARGO_HOME (downloaded crates, binaries installed by 'cargo install')
// stays private to the build, and its bin directory is put first on PATH.
func withRustToolchain(env []string, privateHome string) []string {
	hasRustup := false
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "RUSTUP_HOME="); ok && value != "" {
			hasRustup = true
		}
	}
	if !hasRustup {
		if home, err := os.UserHomeDir(); err == nil {
			if info, statErr := os.Stat(filepath.Join(home, ".rustup")); statErr == nil && info.IsDir() {
				env = append(env, "RUSTUP_HOME="+filepath.Join(home, ".rustup"))
			}
		}
	}
	cargoHome := filepath.Join(privateHome, ".cargo")
	bin := filepath.Join(cargoHome, "bin")
	patched := false
	for i, entry := range env {
		if rest, ok := strings.CutPrefix(entry, "PATH="); ok {
			env[i] = "PATH=" + bin + string(os.PathListSeparator) + rest
			patched = true
		}
	}
	if !patched {
		env = append(env, "PATH="+bin)
	}
	return append(env, "CARGO_HOME="+cargoHome)
}
