package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/MobAI-App/ios-builder/internal/transport"
)

type indexEntry struct {
	Mode string
	SHA  string
	Path string
}

type objectInfo struct {
	Type string
	Size int64
}

// splitLargeFiles replaces regular files that approach GitHub's Git blob size
// limit with a manifest and bounded chunks in the temporary snapshot index.
// The real index and working tree remain untouched.
func splitLargeFiles(ctx context.Context, index, tempDir string) error {
	entries, err := snapshotIndexEntries(ctx, index)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Path == transport.Root || strings.HasPrefix(entry.Path, transport.Root+"/") {
			return fmt.Errorf("source path %q uses reserved snapshot transport namespace", entry.Path)
		}
	}
	if len(entries) == 0 {
		return nil
	}

	objects, err := inspectObjects(ctx, entries)
	if err != nil {
		return err
	}
	manifest := transport.Manifest{Version: transport.Version}
	for _, entry := range entries {
		info := objects[entry.SHA]
		if (entry.Mode != "100644" && entry.Mode != "100755") || info.Type != "blob" || info.Size <= transport.LargeFileThreshold {
			continue
		}
		if !utf8.ValidString(entry.Path) || strings.Contains(entry.Path, "\\") {
			return fmt.Errorf("large source path cannot be represented safely in snapshot transport")
		}
		file, err := splitBlob(ctx, index, tempDir, entry, info.Size)
		if err != nil {
			return fmt.Errorf("split large snapshot file %q: %w", entry.Path, err)
		}
		manifest.Files = append(manifest.Files, file)
	}
	if len(manifest.Files) == 0 {
		return nil
	}

	encoded, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("encode large-file manifest: %w", err)
	}
	manifestFile := filepath.Join(tempDir, "manifest.json")
	if err := os.WriteFile(manifestFile, encoded, 0o600); err != nil {
		return fmt.Errorf("write large-file manifest: %w", err)
	}
	manifestSHA, err := git(ctx, "", "hash-object", "-w", manifestFile)
	if err != nil {
		return err
	}
	if _, err := git(ctx, index, "update-index", "--add", "--cacheinfo", "100644,"+manifestSHA+","+transport.ManifestPath); err != nil {
		return err
	}
	return nil
}

func snapshotIndexEntries(ctx context.Context, index string) ([]indexEntry, error) {
	raw, err := git(ctx, index, "ls-files", "-s", "-z")
	if err != nil {
		return nil, err
	}
	var entries []indexEntry
	for _, record := range strings.Split(raw, "\x00") {
		if record == "" {
			continue
		}
		header, path, ok := strings.Cut(record, "\t")
		fields := strings.Fields(header)
		if !ok || len(fields) != 3 || fields[2] != "0" || path == "" {
			return nil, fmt.Errorf("unexpected Git index entry")
		}
		entries = append(entries, indexEntry{Mode: fields[0], SHA: fields[1], Path: path})
	}
	return entries, nil
}

func inspectObjects(ctx context.Context, entries []indexEntry) (map[string]objectInfo, error) {
	unique := make(map[string]struct{}, len(entries))
	var input strings.Builder
	for _, entry := range entries {
		if _, exists := unique[entry.SHA]; exists {
			continue
		}
		unique[entry.SHA] = struct{}{}
		input.WriteString(entry.SHA)
		input.WriteByte('\n')
	}
	cmd := exec.CommandContext(ctx, "git", "cat-file", "--batch-check=%(objectname) %(objecttype) %(objectsize)")
	cmd.Stdin = strings.NewReader(input.String())
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("inspect snapshot objects: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	objects := make(map[string]objectInfo, len(unique))
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("unexpected Git object metadata")
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid Git object size: %w", err)
		}
		objects[fields[0]] = objectInfo{Type: fields[1], Size: size}
	}
	return objects, nil
}

func splitBlob(ctx context.Context, index, tempDir string, entry indexEntry, size int64) (transport.File, error) {
	blob, err := os.CreateTemp(tempDir, "large-blob-*")
	if err != nil {
		return transport.File{}, err
	}
	blobPath := blob.Name()
	defer func() { _ = os.Remove(blobPath) }()

	fullHash := sha256.New()
	cmd := exec.CommandContext(ctx, "git", "cat-file", "blob", entry.SHA)
	cmd.Stdout = io.MultiWriter(blob, fullHash)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		_ = blob.Close()
		return transport.File{}, fmt.Errorf("read Git blob: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if err := blob.Close(); err != nil {
		return transport.File{}, err
	}
	stat, err := os.Stat(blobPath)
	if err != nil {
		return transport.File{}, err
	}
	if stat.Size() != size {
		return transport.File{}, fmt.Errorf("git blob size changed during snapshot")
	}

	if _, err := git(ctx, index, "update-index", "--force-remove", "--", entry.Path); err != nil {
		return transport.File{}, err
	}
	source, err := os.Open(blobPath)
	if err != nil {
		return transport.File{}, err
	}
	defer source.Close()

	pathDigest := sha256.Sum256([]byte(entry.Path))
	directory := hex.EncodeToString(pathDigest[:16])
	file := transport.File{
		Path:       entry.Path,
		Executable: entry.Mode == "100755",
		Size:       size,
		SHA256:     hex.EncodeToString(fullHash.Sum(nil)),
	}
	for remaining, part := size, 0; remaining > 0; part++ {
		partSize := transport.ChunkSize
		if remaining < partSize {
			partSize = remaining
		}
		chunkFile, err := os.CreateTemp(tempDir, "large-chunk-*")
		if err != nil {
			return transport.File{}, err
		}
		chunkPath := chunkFile.Name()
		chunkHash := sha256.New()
		written, copyErr := io.CopyN(io.MultiWriter(chunkFile, chunkHash), source, partSize)
		closeErr := chunkFile.Close()
		if copyErr != nil || written != partSize || closeErr != nil {
			_ = os.Remove(chunkPath)
			if copyErr != nil {
				return transport.File{}, copyErr
			}
			if closeErr != nil {
				return transport.File{}, closeErr
			}
			return transport.File{}, fmt.Errorf("short chunk write")
		}
		chunkSHA, err := git(ctx, "", "hash-object", "-w", chunkPath)
		_ = os.Remove(chunkPath)
		if err != nil {
			return transport.File{}, err
		}
		transportPath := fmt.Sprintf("%s/%s/%06d", transport.ChunkRoot, directory, part)
		if _, err := git(ctx, index, "update-index", "--add", "--cacheinfo", "100644,"+chunkSHA+","+transportPath); err != nil {
			return transport.File{}, err
		}
		file.Chunks = append(file.Chunks, transport.Chunk{
			Path:   transportPath,
			Size:   partSize,
			SHA256: hex.EncodeToString(chunkHash.Sum(nil)),
		})
		remaining -= partSize
	}
	return file, nil
}
