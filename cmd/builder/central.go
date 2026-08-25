package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/MobAI-App/ios-builder/internal/auth"
	"github.com/MobAI-App/ios-builder/internal/config"
	"github.com/MobAI-App/ios-builder/internal/github"
	"github.com/MobAI-App/ios-builder/internal/registry"
	"github.com/MobAI-App/ios-builder/internal/security"
	"github.com/MobAI-App/ios-builder/internal/snapshot"
	"github.com/spf13/cobra"
)

var centralCmd = &cobra.Command{
	Use:   "central",
	Short: "Configure and diagnose the public central-builder backend",
}

var centralSetupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Configure this private project to use one public builder",
	RunE:  runCentralSetup,
}

var centralDoctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Verify central-builder configuration without printing secrets",
	RunE:  runCentralDoctor,
}

var centralRegisterCmd = &cobra.Command{
	Use:   "register",
	Short: "Register this private project under an opaque ID",
	RunE:  runCentralRegister,
}

func init() {
	rootCmd.AddCommand(centralCmd)
	centralCmd.AddCommand(centralSetupCmd, centralRegisterCmd, centralDoctorCmd)
	centralSetupCmd.Flags().String("builder", "", "Public builder repository as OWNER/REPO (required)")
	centralSetupCmd.Flags().StringP("remote", "r", "origin", "Private source git remote")
	centralSetupCmd.Flags().StringP("project", "p", "", "Project name (defaults to directory name)")
	centralSetupCmd.Flags().String("ios-path", "", "Relative path to the iOS project (auto-detected)")
	centralSetupCmd.Flags().String("scheme", "", "Xcode scheme (auto-detected when empty)")
	centralSetupCmd.Flags().String("configuration", "Debug", "Build configuration: Debug or Release")
	centralSetupCmd.Flags().String("project-id", "", "Existing opaque project ID (normally generated automatically)")
	centralSetupCmd.Flags().String("snapshot-namespace", "", "Existing private snapshot namespace (normally generated automatically)")
	centralRegisterCmd.Flags().String("registry-file", "", "Mode-0600 local registry backup path")
	centralDoctorCmd.Flags().StringP("remote", "r", "origin", "Private source git remote")
	centralDoctorCmd.Flags().Bool("testflight", false, "Also verify apple-production metadata without reading secret values")
}

func runCentralSetup(cmd *cobra.Command, _ []string) error {
	builderName, _ := cmd.Flags().GetString("builder")
	parts := strings.Split(builderName, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return errors.New("--builder OWNER/REPO is required")
	}
	remote, _ := cmd.Flags().GetString("remote")
	sourceOwner, sourceRepo, err := detectGitHubRepo(remote)
	if err != nil {
		return fmt.Errorf("detect private source repository: %w", err)
	}
	project, _ := cmd.Flags().GetString("project")
	if project == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		project = filepath.Base(cwd)
	}
	iosPath, _ := cmd.Flags().GetString("ios-path")
	if iosPath == "" {
		iosPath, _ = detectIOSPath()
	}
	scheme, _ := cmd.Flags().GetString("scheme")
	configuration, _ := cmd.Flags().GetString("configuration")
	recipient, err := security.EnsureIdentity()
	if err != nil {
		return fmt.Errorf("initialize local AGE identity: %w", err)
	}

	projectID, _ := cmd.Flags().GetString("project-id")
	if projectID == "" {
		projectID, err = registry.NewProjectID()
		if err != nil {
			return err
		}
	}
	snapshotNamespace, _ := cmd.Flags().GetString("snapshot-namespace")
	if snapshotNamespace == "" {
		snapshotNamespace, err = registry.NewSnapshotNamespace()
		if err != nil {
			return err
		}
	}
	cfg := &config.Config{
		Project:           project,
		ProjectID:         projectID,
		SnapshotNamespace: snapshotNamespace,
		Platform:          "ios",
		Backend:           config.BackendCentral,
		GitHub:            config.GitHubConfig{Owner: sourceOwner, Repo: sourceRepo},
		Builder:           config.BuilderConfig{Owner: parts[0], Repo: parts[1], Workflow: config.DefaultWorkflow},
		Security:          config.SecurityConfig{Recipient: recipient},
		IOS:               config.IOSConfig{Path: iosPath, Scheme: scheme, Configuration: configuration},
		ReactNative:       config.ReactNativeConfig{Expo: isExpoProject()},
	}
	if isFlutterProject() {
		cfg.Flutter.Version = getLocalFlutterVersion()
	}
	if isKMPProject() {
		cfg.KMP.JDKVersion = "17"
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := config.NewManager().Save(cfg); err != nil {
		return fmt.Errorf("write builder.json: %w", err)
	}

	fmt.Printf("Configured %s/%s to build through %s/%s.\n", sourceOwner, sourceRepo, parts[0], parts[1])
	fmt.Println("No workflow or private source was written to the public builder.")
	printGitHubAppSetup(parts[0], parts[1])
	fmt.Printf("Opaque project ID: %s\n", projectID)
	fmt.Println("After the one-time GitHub App setup, run: builder central register")
	return nil
}

func runCentralRegister(cmd *cobra.Command, _ []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if !cfg.IsCentral() {
		return errors.New("builder.json uses the repository backend")
	}
	if cfg.ProjectID == "" {
		cfg.ProjectID, err = registry.NewProjectID()
		if err != nil {
			return err
		}
	}
	if cfg.SnapshotNamespace == "" {
		cfg.SnapshotNamespace, err = registry.NewSnapshotNamespace()
		if err != nil {
			return err
		}
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	registryPath, _ := cmd.Flags().GetString("registry-file")
	if registryPath == "" {
		registryPath, err = defaultRegistryPath(cfg.Builder.Owner, cfg.Builder.Repo)
		if err != nil {
			return err
		}
	}
	value, err := registry.LoadFile(registryPath)
	if err != nil {
		return fmt.Errorf("load local registry backup: %w", err)
	}
	iosPath := cfg.IOS.Path
	if iosPath == "" {
		iosPath = "."
	}
	configuration := cfg.IOS.Configuration
	if configuration == "" {
		configuration = "Debug"
	}
	token, _, err := auth.GetTokenWithSource()
	if err != nil {
		return err
	}
	client := github.NewClient(token)
	sourceRepository, err := client.GetRepository(cmd.Context(), cfg.GitHub.Owner, cfg.GitHub.Repo)
	if err != nil {
		return fmt.Errorf("read private source repository metadata: %w", err)
	}
	canonical := strings.Split(sourceRepository.FullName, "/")
	if !sourceRepository.Private || len(canonical) != 2 || canonical[0] == "" || canonical[1] == "" {
		return errors.New("central registration requires a private GitHub source repository")
	}
	cfg.GitHub.Owner, cfg.GitHub.Repo = canonical[0], canonical[1]
	project := registry.Project{
		Owner: cfg.GitHub.Owner, Repo: cfg.GitHub.Repo, IOSPath: iosPath,
		Scheme: cfg.IOS.Scheme, Configuration: configuration, FrameworkHint: centralFrameworkHint(cfg),
		SnapshotNamespace: cfg.SnapshotNamespace,
	}
	if err := value.Put(cfg.ProjectID, &project); err != nil {
		return err
	}
	plaintext, err := value.Marshal()
	if err != nil {
		return err
	}
	// Persist the only recoverable plaintext backup before replacing GitHub's
	// write-only secret. A failed remote update can then be retried safely.
	if err := registry.SaveFile(registryPath, plaintext); err != nil {
		return fmt.Errorf("save local registry backup: %w", err)
	}
	if err := config.NewManager().Save(cfg); err != nil {
		return fmt.Errorf("persist opaque project registration: %w", err)
	}
	publicKey, err := client.GetPublicKey(cmd.Context(), cfg.Builder.Owner, cfg.Builder.Repo)
	if err != nil {
		return fmt.Errorf("read builder secret encryption key: %w", err)
	}
	ciphertext, err := github.EncryptSecret(publicKey.Key, string(plaintext))
	if err != nil {
		return err
	}
	if err := client.CreateOrUpdateSecret(cmd.Context(), cfg.Builder.Owner, cfg.Builder.Repo, registry.SecretName, ciphertext, publicKey.KeyID); err != nil {
		return fmt.Errorf("update protected project registry: %w", err)
	}
	fmt.Printf("Registered opaque project %s in %s/%s (registry revision %d).\n", cfg.ProjectID, cfg.Builder.Owner, cfg.Builder.Repo, value.Revision)
	fmt.Printf("Local registry backup: %s (mode 0600; never commit it)\n", registryPath)
	return nil
}

func defaultRegistryPath(owner, repo string) (string, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "ios-builder", "registries", owner+"-"+repo+".json"), nil
}

func centralFrameworkHint(cfg *config.Config) string {
	switch {
	case cfg.ReactNative.Expo:
		return "expo"
	case cfg.Flutter.Version != "":
		return "flutter"
	case cfg.KMP.JDKVersion != "":
		return "kmp"
	default:
		return "auto"
	}
}

func printGitHubAppSetup(owner, repo string) {
	fmt.Println()
	fmt.Println("One-time GitHub App settings (manual browser step):")
	fmt.Println("  App name: ios-cloud-builder-<your-account>")
	fmt.Println("  Homepage URL: https://github.com/" + owner + "/" + repo)
	fmt.Println("  Webhook: inactive")
	fmt.Println("  Repository permissions: Contents = Read-only; Metadata = Read-only (implicit)")
	fmt.Println("  All other repository and organization permissions: No access")
	fmt.Println("  Installation: Only on this account; choose `Only select repositories`")
	fmt.Println("  Generate one private key, then configure the public builder:")
	fmt.Println("    Repository variable APP_CLIENT_ID = the App client ID")
	fmt.Println("    Repository secret   APP_PRIVATE_KEY = the complete generated PEM")
	fmt.Println("  Delete the downloaded PEM after the repository secret is verified, or store it in your password manager.")
}

func runCentralDoctor(cmd *cobra.Command, _ []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if !cfg.IsCentral() {
		return errors.New("builder.json uses the repository backend; run `builder central setup --builder OWNER/REPO`")
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	remote, _ := cmd.Flags().GetString("remote")
	type check struct {
		name string
		fn   func(context.Context) error
	}
	var authSource string
	var client *github.Client
	var publicRepository *github.Repository
	checks := []check{
		{"git executable", func(context.Context) error { _, err := exec.LookPath("git"); return err }},
		{"configuration", func(context.Context) error { return cfg.Validate() }},
		{"local AGE identity", func(context.Context) error {
			_, err := security.LoadIdentity()
			if err != nil {
				return err
			}
			recipient, err := security.Recipient()
			if err != nil {
				return err
			}
			if recipient != cfg.Security.Recipient {
				return errors.New("builder.json recipient does not match the local identity")
			}
			return nil
		}},
		{"GitHub authentication", func(context.Context) error {
			token, source, err := auth.GetTokenWithSource()
			if err != nil {
				return err
			}
			authSource, client = source, github.NewClient(token)
			return nil
		}},
		{"private source API access", func(ctx context.Context) error {
			_, err := client.GetRepository(ctx, cfg.GitHub.Owner, cfg.GitHub.Repo)
			return err
		}},
		{"public builder API access", func(ctx context.Context) error {
			var err error
			publicRepository, err = client.GetRepository(ctx, cfg.Builder.Owner, cfg.Builder.Repo)
			return err
		}},
		{"central workflow", func(ctx context.Context) error {
			return client.GetWorkflow(ctx, cfg.Builder.Owner, cfg.Builder.Repo, cfg.Builder.Workflow)
		}},
		{"APP_CLIENT_ID variable", func(ctx context.Context) error {
			_, err := client.GetActionVariable(ctx, cfg.Builder.Owner, cfg.Builder.Repo, "APP_CLIENT_ID")
			return err
		}},
		{"APP_PRIVATE_KEY secret metadata", func(ctx context.Context) error {
			_, err := client.GetActionSecret(ctx, cfg.Builder.Owner, cfg.Builder.Repo, "APP_PRIVATE_KEY")
			return err
		}},
		{"PROJECT_REGISTRY secret metadata", func(ctx context.Context) error {
			_, err := client.GetActionSecret(ctx, cfg.Builder.Owner, cfg.Builder.Repo, registry.SecretName)
			return err
		}},
		{"source git remote", func(ctx context.Context) error {
			return snapshot.VerifyRemote(ctx, remote, cfg.GitHub.Owner, cfg.GitHub.Repo)
		}},
		{"snapshot push permission (dry run)", func(ctx context.Context) error {
			ref := snapshot.RefForNamespace(cfg.SnapshotNamespace, "00000000-0000-4000-8000-000000000000")
			process := exec.CommandContext(ctx, "git", "push", "--dry-run", remote, "HEAD:"+ref)
			process.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
			if out, err := process.CombinedOutput(); err != nil {
				return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
			}
			return nil
		}},
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	testFlight, _ := cmd.Flags().GetBool("testflight")
	if testFlight {
		checks = append(checks,
			check{"PACKAGING_RECIPIENT variable", func(ctx context.Context) error {
				_, err := client.GetActionVariable(ctx, cfg.Builder.Owner, cfg.Builder.Repo, "PACKAGING_RECIPIENT")
				return err
			}},
			check{"PACKAGING_AGE_IDENTITY secret metadata", func(ctx context.Context) error {
				_, err := client.GetActionSecret(ctx, cfg.Builder.Owner, cfg.Builder.Repo, "PACKAGING_AGE_IDENTITY")
				return err
			}},
			check{"APPLE_SIGNING_RECIPIENT variable", func(ctx context.Context) error {
				_, err := client.GetActionVariable(ctx, cfg.Builder.Owner, cfg.Builder.Repo, "APPLE_SIGNING_RECIPIENT")
				return err
			}},
			check{"apple-production protection rules", func(ctx context.Context) error {
				environment, err := client.GetEnvironment(ctx, cfg.Builder.Owner, cfg.Builder.Repo, "apple-production")
				if err != nil {
					return err
				}
				policies, err := client.GetDeploymentBranchPolicies(ctx, cfg.Builder.Owner, cfg.Builder.Repo, "apple-production")
				if err != nil {
					return err
				}
				if publicRepository == nil || publicRepository.DefaultRef == "" {
					return errors.New("public builder default branch metadata is missing")
				}
				return github.ValidateProductionEnvironment(environment, policies, publicRepository.DefaultRef)
			}},
			check{"APPLE_TEAM_ID environment variable", func(ctx context.Context) error {
				_, err := client.GetEnvironmentActionVariable(ctx, cfg.Builder.Owner, cfg.Builder.Repo, "apple-production", "APPLE_TEAM_ID")
				return err
			}},
		)
		for _, name := range []string{
			"APPLE_SIGNING_AGE_IDENTITY", "APPLE_DISTRIBUTION_P12", "APPLE_DISTRIBUTION_P12_PASSWORD",
			"ASC_API_KEY_P8", "ASC_KEY_ID",
		} {
			secretName := name
			checks = append(checks, check{secretName + " environment secret", func(ctx context.Context) error {
				_, err := client.GetEnvironmentActionSecret(ctx, cfg.Builder.Owner, cfg.Builder.Repo, "apple-production", secretName)
				return err
			}})
		}
		checks = append(checks, check{"provisioning profile environment secret", func(ctx context.Context) error {
			if _, err := client.GetEnvironmentActionSecret(ctx, cfg.Builder.Owner, cfg.Builder.Repo, "apple-production", "APPLE_PROVISIONING_PROFILES"); err == nil {
				return nil
			}
			if _, err := client.GetEnvironmentActionSecret(ctx, cfg.Builder.Owner, cfg.Builder.Repo, "apple-production", "APPLE_PROVISIONING_PROFILE"); err == nil {
				return nil
			}
			return errors.New("configure APPLE_PROVISIONING_PROFILES or legacy APPLE_PROVISIONING_PROFILE")
		}})
	}
	for _, item := range checks {
		if err := item.fn(ctx); err != nil {
			fmt.Printf("FAIL  %s: %v\n", item.name, err)
			return errors.New("central builder doctor found a problem")
		}
		if item.name == "GitHub authentication" {
			fmt.Printf("OK    %s (%s)\n", item.name, authSource)
		} else {
			fmt.Printf("OK    %s\n", item.name)
		}
	}
	if testFlight {
		fmt.Println("Central builder and TestFlight Environment metadata are ready.")
	} else {
		fmt.Println("Central builder is ready.")
	}
	return nil
}
