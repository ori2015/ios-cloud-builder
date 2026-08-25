package snapshot

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MobAI-App/ios-builder/internal/transport"
)

func TestCreateCapturesWorkingTreeWithoutTouchingRepo(t *testing.T) {
	repo := initRepo(t)

	write(t, repo, "tracked.txt", "modified")
	write(t, repo, "untracked.txt", "new")
	write(t, repo, "ignored.txt", "secret")

	statusBefore := run(t, repo, "status", "--porcelain")
	headBefore := run(t, repo, "rev-parse", "HEAD")

	sha, err := Create(context.Background(), "snapshot test")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if got := run(t, repo, "show", sha+":tracked.txt"); got != "modified" {
		t.Errorf("tracked.txt = %q, want %q", got, "modified")
	}
	if got := run(t, repo, "show", sha+":untracked.txt"); got != "new" {
		t.Errorf("untracked.txt = %q, want %q", got, "new")
	}
	if _, err := exec.Command("git", "-C", repo, "show", sha+":ignored.txt").Output(); err == nil {
		t.Error("ignored.txt is in the snapshot, want it excluded by .gitignore")
	}

	if parent := run(t, repo, "rev-parse", sha+"^"); parent != headBefore {
		t.Errorf("parent = %s, want HEAD %s", parent, headBefore)
	}
	if got := run(t, repo, "rev-parse", "HEAD"); got != headBefore {
		t.Errorf("HEAD moved to %s, want %s", got, headBefore)
	}
	if got := run(t, repo, "status", "--porcelain"); got != statusBefore {
		t.Errorf("status = %q, want unchanged %q", got, statusBefore)
	}
}

func TestCreateSplitsLargeFilesIntoBoundedSnapshotChunks(t *testing.T) {
	repo := initRepo(t)
	largePath := filepath.Join(repo, "ios", "Libraries", "libLarge.a")
	if err := os.MkdirAll(filepath.Dir(largePath), 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(largePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(transport.LargeFileThreshold + 1); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	sha, err := Create(context.Background(), "snapshot test")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	paths := run(t, repo, "ls-tree", "-r", "--name-only", sha)
	if strings.Contains(paths, "ios/Libraries/libLarge.a") {
		t.Fatal("large source file remained as an oversized Git blob")
	}
	if !strings.Contains(paths, transport.ManifestPath) || !strings.Contains(paths, transport.ChunkRoot+"/") {
		t.Fatalf("snapshot transport files missing from tree:\n%s", paths)
	}

	rawManifest := run(t, repo, "show", sha+":"+transport.ManifestPath)
	var manifest transport.Manifest
	if err := json.Unmarshal([]byte(rawManifest), &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) != 1 || manifest.Files[0].Path != "ios/Libraries/libLarge.a" || len(manifest.Files[0].Chunks) != 2 {
		t.Fatalf("unexpected transport manifest: %#v", manifest)
	}
	for _, chunk := range manifest.Files[0].Chunks {
		if chunk.Size > transport.ChunkSize {
			t.Fatalf("chunk size %d exceeds limit %d", chunk.Size, transport.ChunkSize)
		}
	}
}

func TestCreateRejectsReservedTransportNamespace(t *testing.T) {
	repo := initRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, transport.Root), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, repo, transport.ManifestPath, "source-owned")
	if _, err := Create(context.Background(), "snapshot test"); err == nil {
		t.Fatal("Create accepted a source file in the reserved transport namespace")
	}
}

func TestParseRepositoryURL(t *testing.T) {
	tests := []struct {
		url       string
		owner     string
		repo      string
		wantValid bool
	}{
		{"https://github.com/acme/private-app.git", "acme", "private-app", true},
		{"git@github.com:acme/private-app.git", "acme", "private-app", true},
		{"git@work-github:acme/private-app.git", "", "", false},
		{"git@evil.example:acme/private-app.git", "", "", false},
		{"ssh://git@github.com/acme/private-app.git", "acme", "private-app", true},
		{"https://example.com/acme/private-app.git", "", "", false},
		{"https://github.com/acme/nested/private-app.git", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			owner, repo, ok := parseRepositoryURL(tt.url)
			if ok != tt.wantValid || owner != tt.owner || repo != tt.repo {
				t.Fatalf("parseRepositoryURL() = %q, %q, %v", owner, repo, ok)
			}
		})
	}
}

func TestDeleteRejectsRefsOutsideNamespace(t *testing.T) {
	err := Delete(context.Background(), "origin", "refs/heads/main")
	if err == nil {
		t.Fatal("Delete accepted a normal branch")
	}
}

func TestCleanupDeletesOnlyMarkedStaleSnapshots(t *testing.T) {
	repo := initRepo(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	run(t, repo, "init", "--bare", remote)
	tree := run(t, repo, "write-tree")
	oldID := "123e4567-e89b-42d3-a456-426614174000"
	oldSHA := commitTreeAt(t, repo, tree, "ios-builder snapshot "+oldID, "2025-01-01T00:00:00Z")
	foreignID := "223e4567-e89b-42d3-a456-426614174000"
	foreignSHA := commitTreeAt(t, repo, tree, "not an ios-builder snapshot", "2025-01-01T00:00:00Z")
	run(t, repo, "push", remote, oldSHA+":"+Ref(oldID), foreignSHA+":"+Ref(foreignID))

	removed, err := Cleanup(context.Background(), remote, 24*time.Hour, time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0].Ref != Ref(oldID) {
		t.Fatalf("removed = %#v", removed)
	}
	refs := run(t, repo, "ls-remote", remote, refPrefix+"*")
	if strings.Contains(refs, Ref(oldID)) || !strings.Contains(refs, Ref(foreignID)) {
		t.Fatalf("unexpected remaining refs: %s", refs)
	}
}

func commitTreeAt(t *testing.T, repo, tree, message, date string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", repo, "commit-tree", tree, "-m", message)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("commit-tree: %v: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func initRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
	} {
		run(t, dir, args...)
	}

	write(t, dir, "tracked.txt", "original")
	write(t, dir, ".gitignore", "ignored.txt\n")
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-m", "initial")

	// Create runs git in the process working directory.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	return dir
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()

	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}

	return strings.TrimSpace(string(out))
}
