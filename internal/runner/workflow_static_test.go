package runner_test

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/MobAI-App/ios-builder/internal/workflow"
	"go.yaml.in/yaml/v3"
)

func TestPublicWorkflowsDisableSetupGoCaches(t *testing.T) {
	t.Parallel()

	var workflowPaths []string
	for _, pattern := range []string{"../../.github/workflows/*.yml", "../../.github/workflows/*.yaml"} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("glob public workflows with %q: %v", pattern, err)
		}
		workflowPaths = append(workflowPaths, matches...)
	}
	if len(workflowPaths) == 0 {
		t.Fatal("no public workflows found")
	}

	for _, workflowPath := range workflowPaths {
		contents, readErr := os.ReadFile(workflowPath)
		if readErr != nil {
			t.Fatalf("read %s: %v", workflowPath, readErr)
		}
		lines := strings.Split(string(contents), "\n")
		for index, line := range lines {
			if !strings.Contains(line, "uses: actions/setup-go@") {
				continue
			}

			stepIndent := len(line) - len(strings.TrimLeft(line, " "))
			cacheDisabled := false
			for next := index + 1; next < len(lines); next++ {
				trimmed := strings.TrimSpace(lines[next])
				indent := len(lines[next]) - len(strings.TrimLeft(lines[next], " "))
				if strings.HasPrefix(trimmed, "- ") && indent <= stepIndent {
					break
				}
				if trimmed == "cache: false" {
					cacheDisabled = true
					break
				}
			}
			if !cacheDisabled {
				t.Errorf("%s:%d setup-go must explicitly set cache: false", workflowPath, index+1)
			}
		}
	}
}

func TestCentralWorkflowSecurityProperties(t *testing.T) {
	rootWorkflow, err := os.ReadFile("../../.github/workflows/ios-build.yml")
	if err != nil {
		t.Fatal(err)
	}
	template, err := workflow.GetCentralWorkflowTemplate()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rootWorkflow, template) {
		t.Fatal("root central workflow and embedded template differ")
	}
	var parsed any
	if err := yaml.Unmarshal(rootWorkflow, &parsed); err != nil {
		t.Fatalf("central workflow is not valid YAML: %v", err)
	}
	// Git for Windows commonly checks text files out with CRLF. Normalize only
	// for semantic assertions; byte identity above still protects the embedded
	// workflow from drifting from the repository copy.
	text := strings.ReplaceAll(string(rootWorkflow), "\r\n", "\n")
	for _, required := range []string{
		"workflow_dispatch:", "permissions:\n  contents: read", "runs-on: macos-15",
		"path: builder", "path: source", "persist-credentials: false",
		"Set up trusted Go toolchain", "go-version-file: builder/go.mod", "cache: false",
		"submodules: false", "lfs: false",
		"APP_CLIENT_ID", "APP_PRIVATE_KEY", "repositories: ${{ inputs.source_repo }}",
		"client-id: ${{ vars.APP_CLIENT_ID }}",
		"skip-token-revoke: true", "permission-contents: read",
		"if: always() && steps.source-token.outcome == 'success'",
		"Restore bounded large-file snapshot chunks", "restore-snapshot --source",
		"CODE_SIGNING_ALLOWED: 'NO'", "retention-days: 1",
		"name: ios-builder-${{ inputs.build_id }}", "encrypted/build.log.age", "encrypted/App.ipa.age",
		"go mod verify",
		"operation:", "environment: apple-production", "APPLE_SIGNING_RECIPIENT",
		"APPLE_SIGNING_AGE_IDENTITY", "APPLE_DISTRIBUTION_P12", "APPLE_PROVISIONING_PROFILE", "APPLE_PROVISIONING_PROFILES",
		"ASC_API_KEY_P8", "deploy-testflight", "--build-number", "github.run_number", "github.run_attempt", "ios-builder-deploy-${{ inputs.build_id }}",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("central workflow missing %q", required)
		}
	}
	deployMarker := "  sign-and-deploy:"
	deployIndex := strings.Index(text, deployMarker)
	if deployIndex < 0 {
		t.Fatal("central workflow has no protected deployment job")
	}
	deployJob := text[deployIndex:]
	for _, forbidden := range []string{
		"path: source", "APP_PRIVATE_KEY", "source-token", "source_owner", "source_repo",
		"App.ipa\n", "App.ipa.age\n          retention-days",
	} {
		if strings.Contains(deployJob, forbidden) {
			t.Errorf("protected deployment job contains forbidden text %q", forbidden)
		}
	}
	for _, forbidden := range []string{
		"pull_request:", "pull_request_target:", "issue_comment:", "workflow_run:",
		"actions/cache", "DerivedData cache", "use_signing", "IOS_CERTIFICATE",
		"GITHUB_STEP_SUMMARY", "eval ", "printenv", "git remote -v", "npm install",
		"app-id:", "encrypted/*.age",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("central workflow contains forbidden text %q", forbidden)
		}
	}
	uses := regexp.MustCompile(`(?m)^\s*uses:\s*[^\s@]+@([^\s]+)$`).FindAllStringSubmatch(text, -1)
	sha := regexp.MustCompile(`^[0-9a-f]{40}$`)
	if len(uses) == 0 {
		t.Fatal("workflow has no actions")
	}
	for _, use := range uses {
		if !sha.MatchString(use[1]) {
			t.Errorf("action is not pinned to a full SHA: %s", use[0])
		}
	}
	ordered := []string{
		"Set up trusted Go toolchain",
		"Build trusted runner before private checkout",
		"Validate all dispatch inputs before credential creation",
		"Create repository-scoped GitHub App token",
		"Checkout exactly the authorized private snapshot",
		"Revoke private repository token before project code",
		"Verify checkout credential cleanup",
		"Restore bounded large-file snapshot chunks",
		"Detect supported iOS framework",
		"Build unsigned IPA and encrypt private outputs",
		"Upload ciphertext only",
	}
	last := -1
	for _, marker := range ordered {
		index := strings.Index(text, marker)
		if index <= last {
			t.Fatalf("workflow boundary %q is missing or out of order", marker)
		}
		last = index
	}
}
