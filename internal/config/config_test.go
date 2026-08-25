package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"
)

func TestConfig_Validate(t *testing.T) {
	tests := []struct {
		name     string
		config   Config
		wantErr  bool
		errField string
	}{
		{
			name: "valid config",
			config: Config{
				Project:  "MyApp",
				Platform: "ios",
				GitHub:   GitHubConfig{Owner: "user", Repo: "builder-myapp"},
			},
			wantErr: false,
		},
		{
			name: "missing project",
			config: Config{
				Platform: "ios",
				GitHub:   GitHubConfig{Owner: "user", Repo: "builder-myapp"},
			},
			wantErr:  true,
			errField: "project",
		},
		{
			name: "missing github owner",
			config: Config{
				Project:  "MyApp",
				Platform: "ios",
				GitHub:   GitHubConfig{Repo: "builder-myapp"},
			},
			wantErr:  true,
			errField: "github.owner",
		},
		{
			name: "missing github repo",
			config: Config{
				Project:  "MyApp",
				Platform: "ios",
				GitHub:   GitHubConfig{Owner: "user"},
			},
			wantErr:  true,
			errField: "github.repo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && err != nil {
				valErr, ok := err.(*ValidationError)
				if !ok {
					t.Errorf("Validate() error type = %T, want *ValidationError", err)
					return
				}
				if valErr.Field != tt.errField {
					t.Errorf("Validate() error field = %q, want %q", valErr.Field, tt.errField)
				}
			}
		})
	}
}

func TestManager_SaveAndLoad(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "builder.json")

	mgr := &Manager{path: configPath}

	// Create config
	cfg := &Config{
		Project:  "TestApp",
		Platform: "ios",
		GitHub: GitHubConfig{
			Owner: "testuser",
			Repo:  "builder-testapp",
		},
	}

	// Save
	if err := mgr.Save(cfg); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// Verify file exists
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		t.Error("Config file not created after Save()")
	}

	// Load
	loaded, err := mgr.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	// Verify fields
	if loaded.Project != cfg.Project {
		t.Errorf("Project = %q, want %q", loaded.Project, cfg.Project)
	}
	if loaded.Platform != cfg.Platform {
		t.Errorf("Platform = %q, want %q", loaded.Platform, cfg.Platform)
	}
	if loaded.GitHub.Owner != cfg.GitHub.Owner {
		t.Errorf("GitHub.Owner = %q, want %q", loaded.GitHub.Owner, cfg.GitHub.Owner)
	}
	if loaded.GitHub.Repo != cfg.GitHub.Repo {
		t.Errorf("GitHub.Repo = %q, want %q", loaded.GitHub.Repo, cfg.GitHub.Repo)
	}
}

func TestManager_Load_NotFound(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "nonexistent.json")

	mgr := &Manager{path: configPath}

	_, err := mgr.Load()
	if err != ErrConfigNotFound {
		t.Errorf("Load() error = %v, want ErrConfigNotFound", err)
	}
}

func TestNewManager(t *testing.T) {
	mgr := NewManager()
	if mgr.path != ConfigFileName {
		t.Errorf("NewManager().path = %q, want %q", mgr.path, ConfigFileName)
	}
}

func TestManager_LoadMigratesLegacyConfig(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "builder.json")
	legacy := []byte(`{
  "project": "LegacyApp",
  "platform": "ios",
  "github": {"owner": "source-owner", "repo": "private-app"}
}`)
	if err := os.WriteFile(configPath, legacy, 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := (&Manager{path: configPath}).Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Backend != BackendRepository {
		t.Errorf("Backend = %q, want %q", cfg.Backend, BackendRepository)
	}
	if cfg.IsCentral() {
		t.Error("legacy config unexpectedly migrated to central mode")
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("migrated legacy Validate() error = %v", err)
	}
}

func TestConfig_CentralDefaultsAndRoundTrip(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Project:           "PrivateApp",
		ProjectID:         "p_0123456789abcdef0123456789abcdef",
		SnapshotNamespace: "11111111111111111111111111111111",
		Platform:          "ios",
		Backend:           BackendCentral,
		GitHub:            GitHubConfig{Owner: "source-owner", Repo: "private-app"},
		Builder:           BuilderConfig{Owner: "builder-owner", Repo: "ios-cloud-builder"},
		Security:          SecurityConfig{Recipient: identity.Recipient().String()},
	}
	if changed := cfg.ApplyDefaults(); !changed {
		t.Fatal("ApplyDefaults() = false, want central workflow default")
	}
	if cfg.Builder.Workflow != DefaultWorkflow {
		t.Errorf("Builder.Workflow = %q, want %q", cfg.Builder.Workflow, DefaultWorkflow)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip Config
	if err := json.Unmarshal(data, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if roundTrip.GitHub.Repo != "private-app" || roundTrip.Builder.Repo != "ios-cloud-builder" {
		t.Fatalf("source/builder repositories not preserved: %#v", roundTrip)
	}
}

func TestConfig_MigratesPreBackendCentralConfig(t *testing.T) {
	cfg := Config{Builder: BuilderConfig{Owner: "builder", Repo: "public"}}
	if !cfg.Migrate() {
		t.Fatal("Migrate() = false, want changes")
	}
	if cfg.Backend != BackendCentral {
		t.Errorf("Backend = %q, want %q", cfg.Backend, BackendCentral)
	}
	if cfg.Builder.Workflow != DefaultWorkflow {
		t.Errorf("Builder.Workflow = %q, want %q", cfg.Builder.Workflow, DefaultWorkflow)
	}
}

func TestConfig_CentralValidation(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	valid := Config{
		Project:           "App",
		ProjectID:         "p_0123456789abcdef0123456789abcdef",
		SnapshotNamespace: "11111111111111111111111111111111",
		Platform:          "ios",
		Backend:           BackendCentral,
		GitHub:            GitHubConfig{Owner: "source", Repo: "private"},
		Builder:           BuilderConfig{Owner: "builder", Repo: "public", Workflow: DefaultWorkflow},
		Security:          SecurityConfig{Recipient: identity.Recipient().String()},
	}

	tests := []struct {
		name  string
		field string
		edit  func(*Config)
	}{
		{name: "builder owner", field: "builder.owner", edit: func(c *Config) { c.Builder.Owner = "" }},
		{name: "builder repo", field: "builder.repo", edit: func(c *Config) { c.Builder.Repo = "" }},
		{name: "builder workflow", field: "builder.workflow", edit: func(c *Config) { c.Builder.Workflow = "" }},
		{name: "workflow path", field: "builder.workflow", edit: func(c *Config) { c.Builder.Workflow = "workflows/ios.yml" }},
		{name: "recipient missing", field: "security.recipient", edit: func(c *Config) { c.Security.Recipient = "" }},
		{name: "recipient invalid", field: "security.recipient", edit: func(c *Config) { c.Security.Recipient = "AGE-SECRET-KEY-NOT-PUBLIC" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			tt.edit(&cfg)
			err := cfg.Validate()
			validationErr, ok := err.(*ValidationError)
			if !ok {
				t.Fatalf("Validate() error = %v (%T), want *ValidationError", err, err)
			}
			if validationErr.Field != tt.field {
				t.Errorf("field = %q, want %q", validationErr.Field, tt.field)
			}
		})
	}
}

func TestConfig_RepositoryModeDoesNotRequireCentralFields(t *testing.T) {
	cfg := Config{
		Project: "Legacy",
		Backend: BackendRepository,
		GitHub:  GitHubConfig{Owner: "owner", Repo: "repo"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}
