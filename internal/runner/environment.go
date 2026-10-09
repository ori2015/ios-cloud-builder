package runner

import (
	"os"
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
		"CARGO_HOME": true, "RUSTUP_HOME": true,
	}
	env := make([]string, 0, len(allowed)+8)
	for _, entry := range os.Environ() {
		name, _, ok := strings.Cut(entry, "=")
		if ok && allowed[name] {
			env = append(env, entry)
		}
	}
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
