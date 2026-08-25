package registry

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func testProject() *Project {
	return &Project{Owner: "private-owner", Repo: "private-repo", IOSPath: "ios", Scheme: "Private App", Configuration: "Release", FrameworkHint: "auto", SnapshotNamespace: "11111111111111111111111111111111"}
}

func TestOpaqueProjectRegistryRoundTripAndUnknownRejection(t *testing.T) {
	id := "p_0123456789abcdef0123456789abcdef"
	value := New()
	if err := value.Put(id, testProject()); err != nil {
		t.Fatal(err)
	}
	data, err := value.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	project, err := parsed.Resolve(id)
	if err != nil || project.Repo != "private-repo" || parsed.Revision != 1 {
		t.Fatalf("Resolve() = %#v, %v", project, err)
	}
	if _, err := parsed.Resolve("p_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err == nil {
		t.Fatal("unknown project ID accepted")
	}
}

func TestRegistryRejectsMalformedAndPrivateMetadataMasks(t *testing.T) {
	for _, data := range []string{
		`{"version":2,"revision":1,"projects":{}}`,
		`{"version":1,"revision":1,"projects":{"private-repo":{"owner":"o","repo":"r"}}}`,
		`{"version":1,"revision":1,"projects":{},"extra":true}`,
	} {
		if _, err := Parse([]byte(data)); err == nil {
			t.Fatalf("malformed registry accepted: %s", data)
		}
	}
	masks := strings.Join(MaskValues(testProject(), "refs/ios-builder/jobs/id"), "\n")
	for _, value := range []string{"private-owner", "private-repo", "private-owner/private-repo", "https://github.com/private-owner/private-repo", "git@github.com:private-owner/private-repo.git", "11111111111111111111111111111111", "Private App", "ios"} {
		if !strings.Contains(masks, value) {
			t.Errorf("mask list missing %q", value)
		}
	}
}

func TestMaskValuesIncludesCanonicalAndLowercaseRepositoryForms(t *testing.T) {
	project := testProject()
	project.Owner = "Private-Owner"
	project.Repo = "Private-Repo"
	masks := strings.Join(MaskValues(project, "refs/ios-builder/jobs/ref"), "\n")
	for _, value := range []string{
		"Private-Owner/Private-Repo",
		"private-owner/private-repo",
		"https://github.com/Private-Owner/Private-Repo.git",
		"https://github.com/private-owner/private-repo.git",
	} {
		if !strings.Contains(masks, value) {
			t.Errorf("mask list missing casing variant %q", value)
		}
	}
}

func TestProjectIDsAreRandomAndConcurrentSafe(t *testing.T) {
	const count = 64
	ids := make(chan string, count)
	var wg sync.WaitGroup
	for range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := NewProjectID()
			if err != nil {
				t.Errorf("NewProjectID(): %v", err)
				return
			}
			ids <- id
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		if !ProjectIDPattern.MatchString(id) || seen[id] {
			t.Fatalf("invalid or duplicate ID %q", id)
		}
		seen[id] = true
	}
}

func TestLocalRegistryFileIsPrivate(t *testing.T) {
	value := New()
	if err := value.Put("p_0123456789abcdef0123456789abcdef", testProject()); err != nil {
		t.Fatal(err)
	}
	data, _ := value.Marshal()
	path := filepath.Join(t.TempDir(), "nested", "registry.json")
	if err := SaveFile(path, data); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.PathSeparator == '\\' {
		t.Skip("Windows does not expose POSIX permission mode bits")
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("registry mode = %v", info.Mode())
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(path); err == nil {
		t.Fatal("world-readable registry backup was accepted")
	}
}

func TestSaveFileDoesNotFollowPreplantedPartialSymlink(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("symlink fixture is POSIX-specific")
	}
	value := New()
	if err := value.Put("p_0123456789abcdef0123456789abcdef", testProject()); err != nil {
		t.Fatal(err)
	}
	data, _ := value.Marshal()
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")
	sentinel := filepath.Join(dir, "sentinel")
	if err := os.WriteFile(sentinel, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sentinel, path+".partial"); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := SaveFile(path, data); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(sentinel)
	if err != nil || string(contents) != "unchanged" {
		t.Fatalf("partial symlink target changed: %q, %v", contents, err)
	}
}
