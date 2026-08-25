// Package snapshot captures the working tree as a Git commit and publishes it
// to a remote ref, leaving HEAD, the current branch and the index untouched.
package snapshot

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// refPrefix is the remote ref namespace holding build snapshots. Refs outside
// refs/heads and refs/tags do not appear as branches and fire no push events.
const refPrefix = "refs/ios-builder/jobs/"

var (
	snapshotIDPattern        = regexp.MustCompile(`^(?:[0-9a-f]{8}|[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12})$`)
	snapshotNamespacePattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// DefaultMaxAge is the age after which a snapshot is considered abandoned.
const DefaultMaxAge = 24 * time.Hour

// RemoteRef describes a temporary snapshot ref advertised by a git remote.
type RemoteRef struct {
	Ref       string
	SHA       string
	CreatedAt time.Time
}

// Ref returns the remote ref that holds the snapshot for a build.
func Ref(buildID string) string {
	return refPrefix + buildID
}

// RefForNamespace hides the real private ref behind a per-project random
// namespace shared only by private local config and the protected registry.
func RefForNamespace(namespace, buildID string) string {
	return refPrefix + namespace + "/" + buildID
}

// VerifyRemote checks that the selected push remote names the configured
// private source repository. It accepts GitHub HTTPS and SSH/SSH-alias forms,
// comparing only the normalized owner/repository path.
func VerifyRemote(ctx context.Context, remote, owner, repo string) error {
	out, err := git(ctx, "", "remote", "get-url", "--push", remote)
	if err != nil {
		return err
	}
	remoteOwner, remoteRepo, ok := parseRepositoryURL(out)
	if !ok || !strings.EqualFold(remoteOwner, owner) || !strings.EqualFold(remoteRepo, repo) {
		return fmt.Errorf("git remote %q does not match configured private source %s/%s", remote, owner, repo)
	}
	return nil
}

func parseRepositoryURL(raw string) (string, string, bool) {
	raw = strings.TrimSpace(strings.TrimSuffix(raw, ".git"))
	var path string
	if rest, ok := strings.CutPrefix(raw, "https://github.com/"); ok {
		path = rest
	} else if rest, ok := strings.CutPrefix(raw, "ssh://git@github.com/"); ok {
		path = rest
	} else if rest, ok := strings.CutPrefix(raw, "git@github.com:"); ok {
		path = rest
	} else {
		return "", "", false
	}
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// Create commits the working tree, including staged, unstaged and untracked
// files, and returns the commit SHA. Files excluded by .gitignore are not
// included. The commit's parent is HEAD, so it is never reachable from any
// local branch.
func Create(ctx context.Context, message string) (string, error) {
	dir, err := os.MkdirTemp("", "ios-builder-index")
	if err != nil {
		return "", fmt.Errorf("failed to create temporary index: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	index := filepath.Join(dir, "index")
	if _, err := git(ctx, index, "read-tree", "HEAD"); err != nil {
		return "", err
	}
	if _, err := git(ctx, index, "add", "-A"); err != nil {
		return "", err
	}
	if err := splitLargeFiles(ctx, index, dir); err != nil {
		return "", err
	}

	tree, err := git(ctx, index, "write-tree")
	if err != nil {
		return "", err
	}

	return git(ctx, index, "commit-tree", tree, "-p", "HEAD", "-m", message)
}

// Push publishes a snapshot commit to ref on the remote.
func Push(ctx context.Context, remote, sha, ref string) error {
	_, err := git(ctx, "", "push", remote, sha+":"+ref)
	return err
}

// Delete removes ref from the remote.
func Delete(ctx context.Context, remote, ref string) error {
	if !strings.HasPrefix(ref, refPrefix) || len(strings.TrimPrefix(ref, refPrefix)) == 0 {
		return fmt.Errorf("ref %q is outside the snapshot namespace", ref)
	}
	_, err := git(ctx, "", "push", "--delete", remote, ref)
	return err
}

// DeleteLease removes ref only if it still points to expectedSHA. This avoids
// deleting a ref that was replaced concurrently after it was inspected.
func DeleteLease(ctx context.Context, remote, ref, expectedSHA string) error {
	if !strings.HasPrefix(ref, refPrefix) || len(strings.TrimPrefix(ref, refPrefix)) == 0 {
		return fmt.Errorf("ref %q is outside the snapshot namespace", ref)
	}
	if len(expectedSHA) != 40 && len(expectedSHA) != 64 {
		return fmt.Errorf("invalid expected snapshot SHA")
	}
	_, err := git(ctx, "", "push", "--force-with-lease="+ref+":"+expectedSHA, remote, ":"+ref)
	return err
}

// ListStale lists remote snapshot refs whose commit timestamp is older than
// maxAge. It fetches missing commit objects into the object database without
// updating a branch, tag, index, HEAD, or the working tree.
func ListStale(ctx context.Context, remote string, maxAge time.Duration, now time.Time) ([]RemoteRef, error) {
	if maxAge <= 0 {
		maxAge = DefaultMaxAge
	}
	out, err := git(ctx, "", "ls-remote", remote, refPrefix+"*")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}

	var stale []RemoteRef
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !strings.HasPrefix(fields[1], refPrefix) {
			continue
		}
		sha, ref := fields[0], fields[1]
		buildID, ok := snapshotBuildID(ref)
		if !ok {
			continue
		}
		if _, err := git(ctx, "", "cat-file", "-e", sha+"^{commit}"); err != nil {
			if _, fetchErr := git(ctx, "", "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", remote, ref); fetchErr != nil {
				return nil, fmt.Errorf("inspect %s: %w", ref, fetchErr)
			}
		}
		ts, err := git(ctx, "", "show", "-s", "--format=%ct", sha)
		if err != nil {
			return nil, fmt.Errorf("read timestamp for %s: %w", ref, err)
		}
		unix, err := strconv.ParseInt(strings.TrimSpace(ts), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid timestamp for %s: %w", ref, err)
		}
		created := time.Unix(unix, 0)
		subject, err := git(ctx, "", "show", "-s", "--format=%s", sha)
		if err != nil {
			return nil, fmt.Errorf("read marker for %s: %w", ref, err)
		}
		if strings.TrimSpace(subject) != "ios-builder snapshot "+buildID {
			continue
		}
		if now.Sub(created) >= maxAge {
			stale = append(stale, RemoteRef{Ref: ref, SHA: sha, CreatedAt: created})
		}
	}
	return stale, nil
}

func snapshotBuildID(ref string) (string, bool) {
	relative := strings.TrimPrefix(ref, refPrefix)
	parts := strings.Split(relative, "/")
	switch {
	case len(parts) == 1 && snapshotIDPattern.MatchString(parts[0]):
		return parts[0], true
	case len(parts) == 2 && snapshotNamespacePattern.MatchString(parts[0]) && snapshotIDPattern.MatchString(parts[1]):
		return parts[1], true
	default:
		return "", false
	}
}

// Cleanup deletes stale snapshot refs and returns the refs successfully
// removed. It stops on the first deletion failure so the caller gets a clear
// error and can retry safely.
func Cleanup(ctx context.Context, remote string, maxAge time.Duration, now time.Time) ([]RemoteRef, error) {
	refs, err := ListStale(ctx, remote, maxAge, now)
	if err != nil {
		return nil, err
	}
	removed := make([]RemoteRef, 0, len(refs))
	for _, ref := range refs {
		if err := DeleteLease(ctx, remote, ref.Ref, ref.SHA); err != nil {
			return removed, err
		}
		removed = append(removed, ref)
	}
	return removed, nil
}

func git(ctx context.Context, index string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	// Without this, a repository using HTTPS without a credential helper blocks
	// on an interactive password prompt instead of failing.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if index != "" {
		cmd.Env = append(cmd.Env, "GIT_INDEX_FILE="+index)
	}

	var stderr strings.Builder
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}

	return strings.TrimSpace(string(out)), nil
}
