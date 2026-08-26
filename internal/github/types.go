package github

import "time"

// Repository represents a GitHub repository
type Repository struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	FullName    string    `json:"full_name"`
	Description string    `json:"description"`
	Private     bool      `json:"private"`
	HTMLURL     string    `json:"html_url"`
	CloneURL    string    `json:"clone_url"`
	SSHURL      string    `json:"ssh_url"`
	DefaultRef  string    `json:"default_branch"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// WorkflowRun represents a GitHub Actions workflow run
type WorkflowRun struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	DisplayTitle string    `json:"display_title"`
	HeadBranch   string    `json:"head_branch"`
	HeadSHA      string    `json:"head_sha"`
	Status       string    `json:"status"`     // queued, in_progress, completed
	Conclusion   string    `json:"conclusion"` // success, failure, cancelled, skipped
	HTMLURL      string    `json:"html_url"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// ActionVariable is repository-level Actions variable metadata. GitHub never
// returns the value from this endpoint, which makes it safe for diagnostics.
type ActionVariable struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ActionSecret is repository-level Actions secret metadata. Secret values are
// intentionally not returned by GitHub.
type ActionSecret struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Environment is deployment-environment metadata used by doctor to verify
// automatic deployment and branch restrictions without accessing secret values.
type Environment struct {
	ID                     int64                   `json:"id"`
	Name                   string                  `json:"name"`
	ProtectionRules        []EnvironmentRule       `json:"protection_rules"`
	DeploymentBranchPolicy *DeploymentBranchPolicy `json:"deployment_branch_policy"`
}

// EnvironmentRule describes one GitHub deployment protection rule.
type EnvironmentRule struct {
	Type              string                `json:"type"`
	PreventSelfReview bool                  `json:"prevent_self_review"`
	Reviewers         []EnvironmentReviewer `json:"reviewers"`
}

// EnvironmentReviewer is deployment reviewer metadata returned by GitHub.
// The reviewer object itself is intentionally omitted because doctor only
// needs to detect whether a manual approval rule exists.
type EnvironmentReviewer struct {
	Type string `json:"type"`
}

// DeploymentBranchPolicy describes whether an Environment accepts protected
// branches or an explicit custom allowlist.
type DeploymentBranchPolicy struct {
	ProtectedBranches    bool `json:"protected_branches"`
	CustomBranchPolicies bool `json:"custom_branch_policies"`
}

// DeploymentBranchPolicyEntry is one custom branch or tag allowlist pattern.
type DeploymentBranchPolicyEntry struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// DeploymentBranchPoliciesResponse is returned by GitHub's branch-policy list endpoint.
type DeploymentBranchPoliciesResponse struct {
	TotalCount     int                           `json:"total_count"`
	BranchPolicies []DeploymentBranchPolicyEntry `json:"branch_policies"`
}

// WorkflowRunsResponse is the response from listing workflow runs
type WorkflowRunsResponse struct {
	TotalCount   int           `json:"total_count"`
	WorkflowRuns []WorkflowRun `json:"workflow_runs"`
}

// Job represents a job within a workflow run
type Job struct {
	ID     int64     `json:"id"`
	Name   string    `json:"name"`
	Status string    `json:"status"`
	Steps  []JobStep `json:"steps"`
}

// JobStep represents a single step within a job
type JobStep struct {
	Name       string    `json:"name"`
	Status     string    `json:"status"`     // queued, in_progress, completed
	Conclusion string    `json:"conclusion"` // success, failure, skipped
	Number     int       `json:"number"`
	StartedAt  time.Time `json:"started_at"`
}

// JobsResponse is the response from listing jobs for a workflow run
type JobsResponse struct {
	TotalCount int   `json:"total_count"`
	Jobs       []Job `json:"jobs"`
}

// WorkflowDispatchRequest is the request body for triggering a workflow
type WorkflowDispatchRequest struct {
	Ref    string            `json:"ref"`
	Inputs map[string]string `json:"inputs,omitempty"`
}

// PublicKey represents a repository's public key for encrypting secrets
type PublicKey struct {
	KeyID string `json:"key_id"`
	Key   string `json:"key"`
}

// CreateSecretRequest is the request body for creating/updating a secret
type CreateSecretRequest struct {
	EncryptedValue string `json:"encrypted_value"`
	KeyID          string `json:"key_id"`
}

// Artifact represents a GitHub Actions artifact
type Artifact struct {
	ID                 int64     `json:"id"`
	NodeID             string    `json:"node_id"`
	Name               string    `json:"name"`
	SizeInBytes        int64     `json:"size_in_bytes"`
	ArchiveDownloadURL string    `json:"archive_download_url"`
	Expired            bool      `json:"expired"`
	Digest             string    `json:"digest"`
	CreatedAt          time.Time `json:"created_at"`
	ExpiresAt          time.Time `json:"expires_at"`
}

// ArtifactsResponse is the response from listing artifacts
type ArtifactsResponse struct {
	TotalCount int        `json:"total_count"`
	Artifacts  []Artifact `json:"artifacts"`
}

// APIError represents an error response from the GitHub API
type APIError struct {
	Message          string `json:"message"`
	DocumentationURL string `json:"documentation_url"`
	Status           string `json:"status"`
}

func (e *APIError) Error() string {
	return e.Message
}
