package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// GetRepository retrieves a repository by owner and name
func (c *Client) GetRepository(ctx context.Context, owner, repo string) (*Repository, error) {
	path := fmt.Sprintf("/repos/%s/%s", owner, repo)

	var repository Repository
	if err := c.do(ctx, path, &repository); err != nil {
		return nil, fmt.Errorf("failed to get repository: %w", err)
	}

	return &repository, nil
}

// GetPublicKey retrieves the repository's public key for encrypting secrets
func (c *Client) GetPublicKey(ctx context.Context, owner, repo string) (*PublicKey, error) {
	path := fmt.Sprintf("/repos/%s/%s/actions/secrets/public-key", owner, repo)

	var key PublicKey
	if err := c.do(ctx, path, &key); err != nil {
		return nil, fmt.Errorf("failed to get public key: %w", err)
	}

	return &key, nil
}

// CreateOrUpdateSecret creates or updates a repository secret
// The value should be encrypted using the repository's public key
func (c *Client) CreateOrUpdateSecret(ctx context.Context, owner, repo, name, encryptedValue, keyID string) error {
	path := fmt.Sprintf("/repos/%s/%s/actions/secrets/%s", owner, repo, name)

	req := CreateSecretRequest{
		EncryptedValue: encryptedValue,
		KeyID:          keyID,
	}

	resp, err := c.request(ctx, "PUT", path, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("failed to create secret: status %d", resp.StatusCode)
	}

	return nil
}

// GetWorkflow verifies that a workflow exists and is readable. The decoded
// body is deliberately discarded; doctor only needs the HTTP status.
func (c *Client) GetWorkflow(ctx context.Context, owner, repo, workflow string) error {
	return c.do(ctx, fmt.Sprintf("/repos/%s/%s/actions/workflows/%s", owner, repo, workflow), nil)
}

// GetActionVariable returns variable metadata without exposing its value.
func (c *Client) GetActionVariable(ctx context.Context, owner, repo, name string) (*ActionVariable, error) {
	var variable ActionVariable
	if err := c.do(ctx, fmt.Sprintf("/repos/%s/%s/actions/variables/%s", owner, repo, name), &variable); err != nil {
		return nil, err
	}
	return &variable, nil
}

// GetActionSecret returns secret metadata. GitHub never returns secret values.
func (c *Client) GetActionSecret(ctx context.Context, owner, repo, name string) (*ActionSecret, error) {
	var secret ActionSecret
	if err := c.do(ctx, fmt.Sprintf("/repos/%s/%s/actions/secrets/%s", owner, repo, name), &secret); err != nil {
		return nil, err
	}
	return &secret, nil
}

// GetEnvironment verifies that a protected deployment environment exists.
func (c *Client) GetEnvironment(ctx context.Context, owner, repo, name string) (*Environment, error) {
	var environment Environment
	if err := c.do(ctx, fmt.Sprintf("/repos/%s/%s/environments/%s", owner, repo, name), &environment); err != nil {
		return nil, err
	}
	return &environment, nil
}

// GetDeploymentBranchPolicies returns the custom branch allowlist for an Environment.
func (c *Client) GetDeploymentBranchPolicies(ctx context.Context, owner, repo, environment string) ([]DeploymentBranchPolicyEntry, error) {
	var response DeploymentBranchPoliciesResponse
	path := fmt.Sprintf("/repos/%s/%s/environments/%s/deployment-branch-policies?per_page=100", owner, repo, environment)
	if err := c.do(ctx, path, &response); err != nil {
		return nil, err
	}
	return response.BranchPolicies, nil
}

// ValidateProductionEnvironment fails closed unless deployment starts without
// a manual approval gate and is restricted to exactly the trusted branch.
func ValidateProductionEnvironment(environment *Environment, policies []DeploymentBranchPolicyEntry, trustedBranch string) error {
	if environment == nil || environment.Name == "" {
		return errors.New("deployment Environment metadata is missing")
	}
	for _, rule := range environment.ProtectionRules {
		if rule.Type == "required_reviewers" {
			return errors.New("manual deployment approval must not be configured")
		}
	}
	branchPolicy := environment.DeploymentBranchPolicy
	if branchPolicy == nil || branchPolicy.ProtectedBranches || !branchPolicy.CustomBranchPolicies {
		return errors.New("deployment must use a custom branch allowlist")
	}
	if len(policies) != 1 || policies[0].Name != trustedBranch || (policies[0].Type != "" && policies[0].Type != "branch") {
		return fmt.Errorf("deployment branch allowlist must contain only branch %q", trustedBranch)
	}
	return nil
}

// GetEnvironmentActionSecret returns environment-secret metadata only.
func (c *Client) GetEnvironmentActionSecret(ctx context.Context, owner, repo, environment, name string) (*ActionSecret, error) {
	var secret ActionSecret
	if err := c.do(ctx, fmt.Sprintf("/repos/%s/%s/environments/%s/secrets/%s", owner, repo, environment, name), &secret); err != nil {
		return nil, err
	}
	return &secret, nil
}

// GetEnvironmentActionVariable returns environment-variable metadata.
func (c *Client) GetEnvironmentActionVariable(ctx context.Context, owner, repo, environment, name string) (*ActionVariable, error) {
	var variable ActionVariable
	if err := c.do(ctx, fmt.Sprintf("/repos/%s/%s/environments/%s/variables/%s", owner, repo, environment, name), &variable); err != nil {
		return nil, err
	}
	return &variable, nil
}
