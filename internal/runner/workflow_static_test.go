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
		"APP_CLIENT_ID", "APP_PRIVATE_KEY", "PROJECT_REGISTRY", "project_id:",
		"repositories: ${{ steps.project.outputs.source_repo }}",
		"client-id: ${{ vars.APP_CLIENT_ID }}",
		"skip-token-revoke: true", "permission-contents: read",
		"if: always() && steps.source-token.outcome == 'success'",
		"Restore bounded large-file snapshot chunks", "restore-snapshot --source",
		"CODE_SIGNING_ALLOWED: 'NO'", "retention-days: 1",
		"name: ios-builder-${{ inputs.build_id }}", "encrypted/build.log.age", "encrypted/App.ipa.age", "encrypted/project-output.age",
		"go mod verify",
		"operation:", "environment: apple-production", "PACKAGING_RECIPIENT", "PACKAGING_AGE_IDENTITY", "APPLE_SIGNING_RECIPIENT",
		"actions/attest@59d89421af93a897026c735860bf21b6eb4f7b26", "verify-provenance", "provenance.json",
		"APPLE_SIGNING_AGE_IDENTITY", "APPLE_DISTRIBUTION_P12", "APPLE_PROVISIONING_PROFILE", "APPLE_PROVISIONING_PROFILES",
		"ASC_API_KEY_P8", "deploy-testflight", "--build-number auto", "ios-builder-deploy-${{ inputs.build_id }}",
		"TESTFLIGHT_BETA_GROUPS: ${{ secrets.TESTFLIGHT_BETA_GROUPS }}",
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
		"TESTFLIGHT_BETA_GROUPS: ${{ vars.TESTFLIGHT_BETA_GROUPS }}",
	} {
		if strings.Contains(deployJob, forbidden) {
			t.Errorf("protected deployment job contains forbidden text %q", forbidden)
		}
	}
	signMarker := "      - name: Sign and upload directly to App Store Connect"
	signIndex := strings.Index(deployJob, signMarker)
	provenanceIndex := strings.Index(deployJob, "      - name: Verify provenance before loading Apple credentials")
	if provenanceIndex < 0 || signIndex < 0 || provenanceIndex >= signIndex {
		t.Fatal("provenance verification does not precede the Apple-secret step")
	}
	beforeAppleStep := deployJob[:signIndex]
	for _, forbidden := range []string{
		"secrets.APPLE_SIGNING_AGE_IDENTITY", "secrets.APPLE_DISTRIBUTION_P12",
		"secrets.APPLE_PROVISIONING_PROFILE", "secrets.APPLE_PROVISIONING_PROFILES",
		"secrets.ASC_API_KEY_P8", "secrets.ASC_KEY_ID", "secrets.ASC_ISSUER_ID",
	} {
		if strings.Contains(beforeAppleStep, forbidden) {
			t.Errorf("Apple secret %q is available before provenance verification", forbidden)
		}
	}
	packageMarker := "  trusted-package:"
	packageIndex := strings.Index(text, packageMarker)
	if packageIndex < 0 || packageIndex >= deployIndex {
		t.Fatal("isolated trusted packaging job is missing")
	}
	packageJob := text[packageIndex:deployIndex]
	if strings.Contains(packageJob, "path: source") || strings.Contains(packageJob, "APP_PRIVATE_KEY") {
		t.Fatal("trusted packaging job can access private source credentials or checkout")
	}
	if strings.Count(text, "id-token: write") != 1 || strings.Count(text, "attestations: write") != 1 {
		t.Fatal("attestation permissions are not isolated to one job")
	}
	telegramToken := regexp.MustCompile(`\b[0-9]{6,}:[A-Za-z0-9_-]{20,}\b`)
	if telegramToken.MatchString(text) {
		t.Error("central workflow contains a plaintext Telegram bot token")
	}
	for _, forbidden := range []string{
		"pull_request:", "pull_request_target:", "issue_comment:", "workflow_run:",
		"actions/cache", "DerivedData cache", "use_signing", "IOS_CERTIFICATE",
		"GITHUB_STEP_SUMMARY", "eval ", "printenv", "git remote -v", "npm install",
		"app-id:", "encrypted/*.age", "notify-approval:", "TELEGRAM_BOT_TOKEN",
		"TELEGRAM_CHAT_ID", "APPROVAL_URL", "approval required",
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
		"Resolve opaque project and mask private metadata",
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
	permissionsIndex := strings.Index(text, "permissions:")
	if permissionsIndex < 0 {
		t.Fatal("workflow has no permissions boundary")
	}
	dispatchSchema := text[:permissionsIndex]
	for _, forbidden := range []string{"source_owner:", "source_repo:", "snapshot_ref:", "ios_path:", "scheme:", "configuration:", "framework_hint:"} {
		if strings.Contains(dispatchSchema, forbidden) {
			t.Errorf("workflow dispatch still exposes private metadata input %q", forbidden)
		}
	}
}
