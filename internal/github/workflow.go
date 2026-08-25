package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

var (
	// ErrWorkflowRunNotFound indicates that no run had the exact build ID.
	ErrWorkflowRunNotFound = errors.New("workflow run not found")
	// ErrArtifactNotFound indicates that an exact artifact name is not yet
	// present in the selected workflow run.
	ErrArtifactNotFound = errors.New("artifact not found")
)

const (
	// workflowStartPollInterval is the polling interval when waiting for workflow to start.
	workflowStartPollInterval = 2 * time.Second
	workflowPollInterval      = 5 * time.Second
	maxWorkflowRunPages       = 10
)

// TriggerWorkflow triggers a workflow_dispatch event
func (c *Client) TriggerWorkflow(ctx context.Context, owner, repo, workflowFile string, inputs map[string]string) error {
	path := fmt.Sprintf("/repos/%s/%s/actions/workflows/%s/dispatches", owner, repo, workflowFile)

	// Get the default branch
	repository, err := c.GetRepository(ctx, owner, repo)
	if err != nil {
		return fmt.Errorf("failed to get repository: %w", err)
	}

	ref := repository.DefaultRef
	if ref == "" {
		ref = "main"
	}

	req := WorkflowDispatchRequest{
		Ref:    ref,
		Inputs: inputs,
	}

	resp, err := c.request(ctx, "POST", path, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to trigger workflow (status %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return nil
}

// CancelWorkflowRun requests cancellation of a run. Best-effort: a run that has
// already finished returns 409, which is treated as success (nothing to cancel).
func (c *Client) CancelWorkflowRun(ctx context.Context, owner, repo string, runID int64) error {
	path := fmt.Sprintf("/repos/%s/%s/actions/runs/%d/cancel", owner, repo, runID)

	resp, err := c.request(ctx, "POST", path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// 202 Accepted on success; 409 Conflict when the run already completed.
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to cancel run (status %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return nil
}

// GetWorkflowRun retrieves a specific workflow run
func (c *Client) GetWorkflowRun(ctx context.Context, owner, repo string, runID int64) (*WorkflowRun, error) {
	path := fmt.Sprintf("/repos/%s/%s/actions/runs/%d", owner, repo, runID)

	var run WorkflowRun
	if err := c.do(ctx, path, &run); err != nil {
		return nil, fmt.Errorf("failed to get workflow run: %w", err)
	}

	return &run, nil
}

// ListWorkflowRuns lists workflow runs for a workflow file
func (c *Client) ListWorkflowRuns(ctx context.Context, owner, repo, workflowFile string) ([]WorkflowRun, error) {
	var runs []WorkflowRun
	for page := 1; page <= maxWorkflowRunPages; page++ {
		path := fmt.Sprintf("/repos/%s/%s/actions/workflows/%s/runs?event=workflow_dispatch&per_page=100&page=%d", owner, repo, workflowFile, page)
		var resp WorkflowRunsResponse
		if err := c.do(ctx, path, &resp); err != nil {
			return nil, fmt.Errorf("failed to list workflow runs: %w", err)
		}
		runs = append(runs, resp.WorkflowRuns...)
		if len(resp.WorkflowRuns) < 100 || (resp.TotalCount > 0 && len(runs) >= resp.TotalCount) {
			return runs, nil
		}
	}
	return nil, fmt.Errorf("workflow run listing exceeded bounded %d-page search", maxWorkflowRunPages)
}

// FindWorkflowRunByBuildID finds the run whose name carries the build ID. The
// workflow puts the ID in run-name so concurrent builds cannot pick up each
// other's runs.
func (c *Client) FindWorkflowRunByBuildID(ctx context.Context, owner, repo, workflowFile, buildID string) (*WorkflowRun, error) {
	runs, err := c.ListWorkflowRuns(ctx, owner, repo, workflowFile)
	if err != nil {
		return nil, err
	}

	var match *WorkflowRun
	for i := range runs {
		// display_title is the workflow run-name. Keep Name as a compatibility
		// fallback for older GitHub API fixtures, but require a token boundary so
		// one build ID can never be a substring match for another.
		if runTitleMatchesBuildID(runs[i].DisplayTitle, workflowFile, buildID) ||
			(runs[i].DisplayTitle == "" && runTitleMatchesBuildID(runs[i].Name, workflowFile, buildID)) {
			if match != nil {
				return nil, fmt.Errorf("multiple workflow runs found for build %s", buildID)
			}
			match = &runs[i]
		}
	}
	if match != nil {
		return match, nil
	}

	return nil, fmt.Errorf("%w for build %s", ErrWorkflowRunNotFound, buildID)
}

// PollForWorkflowCompletion waits for a specific, already-correlated run to
// complete. The run ID (rather than another list/search) is used throughout so
// a concurrent dispatch can never be adopted.
func (c *Client) PollForWorkflowCompletion(ctx context.Context, owner, repo string, runID int64, timeout time.Duration, onPoll func()) (*WorkflowRun, error) {
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for workflow run %d to complete", runID)
		}
		run, err := c.GetWorkflowRun(ctx, owner, repo, runID)
		if err != nil {
			return nil, err
		}
		if run.Status == "completed" {
			return run, nil
		}
		if onPoll != nil {
			onPoll()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(workflowPollInterval):
		}
	}
}

func runTitleMatchesBuildID(title, workflowFile, buildID string) bool {
	prefix := "iOS Build "
	if workflowFile == "ios-share.yml" {
		prefix = "iOS Simulator "
	}
	return buildID != "" && title == prefix+buildID
}

// PollForWorkflowStart polls until the run for buildID appears
func (c *Client) PollForWorkflowStart(ctx context.Context, owner, repo, workflowFile, buildID string, timeout time.Duration) (*WorkflowRun, error) {
	deadline := time.Now().Add(timeout)

	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for workflow to start")
		}

		run, err := c.FindWorkflowRunByBuildID(ctx, owner, repo, workflowFile, buildID)
		if err == nil {
			return run, nil
		}
		if !errors.Is(err, ErrWorkflowRunNotFound) {
			return nil, err
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(workflowStartPollInterval):
		}
	}
}

// ListRunJobs lists the jobs and their steps for a workflow run
func (c *Client) ListRunJobs(ctx context.Context, owner, repo string, runID int64) ([]Job, error) {
	path := fmt.Sprintf("/repos/%s/%s/actions/runs/%d/jobs", owner, repo, runID)

	var resp JobsResponse
	if err := c.do(ctx, path, &resp); err != nil {
		return nil, fmt.Errorf("failed to list run jobs: %w", err)
	}

	return resp.Jobs, nil
}

// RunningStep returns the step currently executing in a run, and the total
// number of steps in its job. It returns nil when no step is running.
func (c *Client) RunningStep(ctx context.Context, owner, repo string, runID int64) (*JobStep, int, error) {
	jobs, err := c.ListRunJobs(ctx, owner, repo, runID)
	if err != nil {
		return nil, 0, err
	}

	for _, job := range jobs {
		for i := range job.Steps {
			if job.Steps[i].Status == "in_progress" {
				return &job.Steps[i], len(job.Steps), nil
			}
		}
	}

	return nil, 0, nil
}

// ListRunArtifacts lists all artifacts for a workflow run
func (c *Client) ListRunArtifacts(ctx context.Context, owner, repo string, runID int64) ([]Artifact, error) {
	path := fmt.Sprintf("/repos/%s/%s/actions/runs/%d/artifacts", owner, repo, runID)

	var resp ArtifactsResponse
	if err := c.do(ctx, path, &resp); err != nil {
		return nil, fmt.Errorf("failed to list artifacts: %w", err)
	}

	return resp.Artifacts, nil
}

// ProgressFunc is called during download with bytes downloaded and total size
type ProgressFunc func(downloaded, total int64)

// DownloadArtifactWithProgress downloads an artifact with progress reporting
func (c *Client) DownloadArtifactWithProgress(ctx context.Context, owner, repo string, artifactID int64, progress ProgressFunc) ([]byte, error) {
	return c.DownloadArtifactWithProgressLimit(ctx, owner, repo, artifactID, 0, progress)
}

// DownloadArtifactWithProgressLimit downloads an artifact while enforcing a
// maximum archive size. A non-positive maxBytes preserves the legacy
// unbounded behavior used by the repository backend.
func (c *Client) DownloadArtifactWithProgressLimit(ctx context.Context, owner, repo string, artifactID, maxBytes int64, progress ProgressFunc) ([]byte, error) {
	path := fmt.Sprintf("/repos/%s/%s/actions/artifacts/%d/zip", owner, repo, artifactID)

	resp, err := c.request(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusFound {
		// Close initial response before following redirect
		resp.Body.Close()

		redirectURL := resp.Header.Get("Location")
		if redirectURL == "" {
			return nil, fmt.Errorf("artifact redirect missing Location header")
		}

		req, err := http.NewRequestWithContext(ctx, "GET", redirectURL, nil)
		if err != nil {
			return nil, err
		}

		resp, err = c.httpClient.Do(req)
		if err != nil {
			return nil, err
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to download artifact: status %d", resp.StatusCode)
	}

	total := resp.ContentLength
	if maxBytes > 0 && total > maxBytes {
		return nil, fmt.Errorf("artifact archive is too large: %d bytes (limit %d)", total, maxBytes)
	}

	reader := io.Reader(resp.Body)
	if progress != nil {
		reader = &progressReader{reader: reader, total: total, progress: progress}
	}
	if maxBytes > 0 {
		reader = io.LimitReader(reader, maxBytes+1)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read artifact data: %w", err)
	}
	if maxBytes > 0 && int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("artifact archive exceeds %d byte limit", maxBytes)
	}

	return data, nil
}

type progressReader struct {
	reader     io.Reader
	total      int64
	downloaded int64
	progress   ProgressFunc
}

func (r *progressReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.downloaded += int64(n)
		r.progress(r.downloaded, r.total)
	}
	return n, err
}

// FindArtifactByName finds an artifact by name in a workflow run
func (c *Client) FindArtifactByName(ctx context.Context, owner, repo string, runID int64, name string) (*Artifact, error) {
	artifacts, err := c.ListRunArtifacts(ctx, owner, repo, runID)
	if err != nil {
		return nil, err
	}

	var match *Artifact
	for i := range artifacts {
		a := &artifacts[i]
		if a.Name == name {
			if a.Expired {
				return nil, fmt.Errorf("artifact %q is expired", name)
			}
			if match != nil {
				return nil, fmt.Errorf("multiple artifacts named %q found in workflow run %d", name, runID)
			}
			match = a
		}
	}
	if match != nil {
		return match, nil
	}

	return nil, fmt.Errorf("%w: %q", ErrArtifactNotFound, name)
}

// PollForRunArtifact waits for an exact artifact name within one exact run.
// Unlike PollForArtifact it is safe to call after a failed run has completed,
// which is required to retrieve encrypted diagnostics uploaded with always().
func (c *Client) PollForRunArtifact(ctx context.Context, owner, repo string, runID int64, artifactName string, timeout time.Duration) (*Artifact, error) {
	deadline := time.Now().Add(timeout)
	for {
		artifact, err := c.FindArtifactByName(ctx, owner, repo, runID, artifactName)
		if err == nil {
			return artifact, nil
		}
		if !errors.Is(err, ErrArtifactNotFound) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for artifact %q in workflow run %d", artifactName, runID)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(workflowPollInterval):
		}
	}
}

// PollForArtifact polls until an artifact with the given name appears in a workflow run.
// This allows downloading the artifact as soon as it's uploaded, without waiting for the
// entire workflow to complete. onPoll, if non-nil, runs once per attempt.
func (c *Client) PollForArtifact(ctx context.Context, owner, repo string, runID int64, artifactName string, timeout time.Duration, onPoll func()) (*Artifact, error) {
	deadline := time.Now().Add(timeout)
	// Use fixed 5s interval (no backoff) to catch artifact quickly after upload
	const artifactPollInterval = 5 * time.Second

	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for artifact %q", artifactName)
		}

		// Check if artifact is available
		artifact, err := c.FindArtifactByName(ctx, owner, repo, runID, artifactName)
		if err == nil {
			return artifact, nil
		}
		if !errors.Is(err, ErrArtifactNotFound) {
			return nil, err
		}

		// Check if workflow failed (no point waiting for artifact)
		run, err := c.GetWorkflowRun(ctx, owner, repo, runID)
		if err != nil {
			return nil, fmt.Errorf("failed to check workflow status: %w", err)
		}
		if run.Status == "completed" && run.Conclusion != "success" {
			return nil, fmt.Errorf("workflow failed with conclusion: %s", run.Conclusion)
		}

		if onPoll != nil {
			onPoll()
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(artifactPollInterval):
			// Fixed interval - no backoff to catch artifact quickly
		}
	}
}

// DeleteArtifact removes an Actions artifact after it has been downloaded.
// Callers generally treat failure as non-fatal because encrypted artifacts
// have a short retention period and contain ciphertext only.
func (c *Client) DeleteArtifact(ctx context.Context, owner, repo string, artifactID int64) error {
	path := fmt.Sprintf("/repos/%s/%s/actions/artifacts/%d", owner, repo, artifactID)
	resp, err := c.request(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to delete artifact (status %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// DeleteRunArtifactsByName removes only exact artifact names from one exact
// workflow run. It is used by cancellation cleanup so concurrent builds and
// unrelated artifacts cannot be affected.
func (c *Client) DeleteRunArtifactsByName(ctx context.Context, owner, repo string, runID int64, names ...string) error {
	artifacts, err := c.ListRunArtifacts(ctx, owner, repo, runID)
	if err != nil {
		return err
	}
	wanted := make(map[string]struct{}, len(names))
	for _, name := range names {
		wanted[name] = struct{}{}
	}
	var deleteErrors []error
	for index := range artifacts {
		artifact := &artifacts[index]
		if _, ok := wanted[artifact.Name]; !ok {
			continue
		}
		if err := c.DeleteArtifact(ctx, owner, repo, artifact.ID); err != nil {
			deleteErrors = append(deleteErrors, fmt.Errorf("delete artifact %d: %w", artifact.ID, err))
		}
	}
	return errors.Join(deleteErrors...)
}
