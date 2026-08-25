package config

import (
	"path"
	"regexp"
	"strings"

	"filippo.io/age"
	"github.com/MobAI-App/ios-builder/internal/registry"
)

var (
	ownerPattern      = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
	repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
	workflowPattern   = regexp.MustCompile(`^[A-Za-z0-9_.-]+\.ya?ml$`)
	schemePattern     = regexp.MustCompile(`^$|^[A-Za-z0-9][A-Za-z0-9 ._+()-]{0,127}$`)
	projectIDPattern  = registry.ProjectIDPattern
)

// Backend identifies where a build workflow is executed.
type Backend string

const (
	// BackendRepository preserves the original ios-builder behavior: the
	// workflow lives in the source repository itself.
	BackendRepository Backend = "repository"
	// BackendCentral dispatches the build to a separate public builder
	// repository. GitHub still identifies the private source repository.
	BackendCentral Backend = "central"

	// DefaultWorkflow is the workflow file used by the central builder unless
	// the project explicitly selects another one.
	DefaultWorkflow = "ios-build.yml"
)

// Config represents the builder.json configuration file
type Config struct {
	Project           string            `json:"project"`
	ProjectID         string            `json:"project_id,omitempty"`
	SnapshotNamespace string            `json:"snapshot_namespace,omitempty"`
	Platform          string            `json:"platform"`
	Backend           Backend           `json:"backend,omitempty"`
	GitHub            GitHubConfig      `json:"github"`
	Builder           BuilderConfig     `json:"builder,omitempty"`
	Security          SecurityConfig    `json:"security,omitempty"`
	IOS               IOSConfig         `json:"ios,omitempty"`
	Flutter           FlutterConfig     `json:"flutter,omitempty"`
	ReactNative       ReactNativeConfig `json:"reactNative,omitempty"`
	KMP               KMPConfig         `json:"kmp,omitempty"`
	MobAI             MobAIConfig       `json:"mobai,omitempty"`
}

// FlutterConfig holds Flutter-specific settings
type FlutterConfig struct {
	Version string      `json:"version,omitempty"` // Pinned Flutter version (e.g., "3.24.0")
	Watch   WatchConfig `json:"watch,omitempty"`   // File watcher settings for hot reload
}

// WatchConfig holds file watcher settings for hot reload
type WatchConfig struct {
	Dirs     []string `json:"dirs,omitempty"`     // Directories to watch
	Patterns []string `json:"patterns,omitempty"` // File patterns to match (by suffix)
	Ignore   []string `json:"ignore,omitempty"`   // Patterns to ignore (by suffix)
	Debounce int      `json:"debounce,omitempty"` // Debounce ms
}

// ReactNativeConfig holds React Native-specific settings
type ReactNativeConfig struct {
	MetroPort int  `json:"metroPort,omitempty"` // Metro bundler port (default: 8081)
	Expo      bool `json:"expo,omitempty"`      // Whether this is an Expo project
}

// KMPConfig holds Kotlin Multiplatform-specific settings.
// The iOS app is built with xcodebuild, which invokes Gradle (via the Xcode
// "Run Script" build phase, or CocoaPods) to compile the shared Kotlin
// framework. The CI build therefore needs a JDK available for Gradle.
type KMPConfig struct {
	JDKVersion string `json:"jdkVersion,omitempty"` // JDK version for Gradle builds (default: 17)
}

// IOSConfig holds iOS build settings
type IOSConfig struct {
	// Path to iOS project relative to repo root (e.g., "ios" for React Native, "platforms/ios" for Cordova)
	// Empty means root directory contains the Xcode project
	Path          string `json:"path,omitempty"`
	Scheme        string `json:"scheme,omitempty"`        // Xcode scheme to build (auto-detected if empty)
	Signing       bool   `json:"signing,omitempty"`       // Whether code signing is configured
	Configuration string `json:"configuration,omitempty"` // Build configuration: Debug (faster) or Release (production)
}

// MobAIConfig holds MobAI settings for local development
type MobAIConfig struct {
	URL      string `json:"url,omitempty"`       // MobAI API URL (default: http://localhost:8686)
	DeviceID string `json:"device_id,omitempty"` // Preferred device ID (default: first available)
}

// GitHubConfig holds GitHub repository settings
type GitHubConfig struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
}

// BuilderConfig identifies the public repository that owns the central build
// workflow. It is distinct from GitHubConfig, which always identifies the
// private source repository in central mode.
type BuilderConfig struct {
	Owner    string `json:"owner,omitempty"`
	Repo     string `json:"repo,omitempty"`
	Workflow string `json:"workflow,omitempty"`
}

// SecurityConfig contains public cryptographic configuration. Recipient is an
// AGE X25519 recipient; the corresponding identity is local-only and must
// never be written to builder.json.
type SecurityConfig struct {
	Recipient string `json:"recipient,omitempty"`
}

// ApplyDefaults migrates an in-memory configuration to the current schema.
// It returns true when a value changed. Configurations written by upstream
// versions did not have a backend, and therefore retain repository behavior.
func (c *Config) ApplyDefaults() bool {
	changed := false
	if c.Backend == "" {
		// No upstream release had builder/security fields, so their presence
		// unambiguously identifies a pre-backend central configuration.
		if strings.TrimSpace(c.Builder.Owner) != "" ||
			strings.TrimSpace(c.Builder.Repo) != "" ||
			strings.TrimSpace(c.Builder.Workflow) != "" ||
			strings.TrimSpace(c.Security.Recipient) != "" {
			c.Backend = BackendCentral
		} else {
			c.Backend = BackendRepository
		}
		changed = true
	}
	if c.Backend == BackendCentral && strings.TrimSpace(c.Builder.Workflow) == "" {
		c.Builder.Workflow = DefaultWorkflow
		changed = true
	}
	return changed
}

// Migrate is an alias for ApplyDefaults, provided for callers that explicitly
// migrate and persist an existing builder.json.
func (c *Config) Migrate() bool { return c.ApplyDefaults() }

// IsCentral reports whether the separate central-builder backend is selected.
func (c *Config) IsCentral() bool { return c.Backend == BackendCentral }

// Validate checks if the configuration is valid
func (c *Config) Validate() error {
	if strings.TrimSpace(c.Project) == "" || len(c.Project) > 128 || strings.ContainsAny(c.Project, "\r\n\x00") {
		return &ValidationError{Field: "project", Message: "project name is required"}
	}
	if !validOwner(c.GitHub.Owner) {
		return &ValidationError{Field: "github.owner", Message: "must be a valid GitHub owner"}
	}
	if !validRepository(c.GitHub.Repo) {
		return &ValidationError{Field: "github.repo", Message: "must be a valid GitHub repository name"}
	}
	if c.Backend != "" && c.Backend != BackendRepository && c.Backend != BackendCentral {
		return &ValidationError{Field: "backend", Message: "must be repository or central"}
	}
	if c.Backend == BackendCentral {
		if !projectIDPattern.MatchString(c.ProjectID) {
			return &ValidationError{Field: "project_id", Message: "run `builder central register` to create an opaque project ID"}
		}
		if !registry.SnapshotNamespacePattern.MatchString(c.SnapshotNamespace) {
			return &ValidationError{Field: "snapshot_namespace", Message: "run `builder central register` to create a private snapshot namespace"}
		}
		if !validOwner(c.Builder.Owner) {
			return &ValidationError{Field: "builder.owner", Message: "must be a valid GitHub owner"}
		}
		if !validRepository(c.Builder.Repo) {
			return &ValidationError{Field: "builder.repo", Message: "must be a valid GitHub repository name"}
		}
		if len(c.Builder.Workflow) > 255 || !workflowPattern.MatchString(c.Builder.Workflow) {
			return &ValidationError{Field: "builder.workflow", Message: "must be a workflow YAML file name, not a path"}
		}
		if strings.TrimSpace(c.Security.Recipient) == "" {
			return &ValidationError{Field: "security.recipient", Message: "AGE recipient is required in central mode"}
		}
		if _, err := age.ParseX25519Recipient(strings.TrimSpace(c.Security.Recipient)); err != nil {
			return &ValidationError{Field: "security.recipient", Message: "must be a valid AGE X25519 recipient"}
		}
	}
	if !validIOSPath(c.IOS.Path) {
		return &ValidationError{Field: "ios.path", Message: "must be a clean relative path without traversal or backslashes"}
	}
	if !schemePattern.MatchString(c.IOS.Scheme) {
		return &ValidationError{Field: "ios.scheme", Message: "contains unsupported characters or is too long"}
	}
	if c.IOS.Configuration != "" && c.IOS.Configuration != "Debug" && c.IOS.Configuration != "Release" {
		return &ValidationError{Field: "ios.configuration", Message: "must be Debug or Release"}
	}
	return nil
}

func validOwner(value string) bool {
	return ownerPattern.MatchString(value) && !strings.Contains(value, "--")
}

func validRepository(value string) bool {
	return repositoryPattern.MatchString(value) && value != "." && value != ".." && !strings.HasSuffix(strings.ToLower(value), ".git")
}

func validIOSPath(value string) bool {
	if value == "" || value == "." {
		return true
	}
	if strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\\\r\n\x00") || len(value) > 512 {
		return false
	}
	return path.Clean(value) == value && value != ".." && !strings.HasPrefix(value, "../")
}

// ValidationError represents a configuration validation error
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return "config validation error: " + e.Field + ": " + e.Message
}
