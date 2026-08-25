package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/MobAI-App/ios-builder/internal/transport"
)

func TestRestoreLargeSnapshotFiles(t *testing.T) {
	root := t.TempDir()
	transportDir := filepath.Join(root, filepath.FromSlash(transport.ChunkRoot), "file")
	if err := os.MkdirAll(transportDir, 0o755); err != nil {
		t.Fatal(err)
	}

	fullHash := sha256.New()
	manifestFile := transport.File{
		Path:       "ios/Libraries/libLarge.a",
		Executable: false,
		Size:       transport.LargeFileThreshold + 1,
	}
	remaining := manifestFile.Size
	buffer := make([]byte, 1024*1024)
	for i := range buffer {
		buffer[i] = byte(i % 251)
	}
	for part := 0; remaining > 0; part++ {
		partSize := transport.ChunkSize
		if remaining < partSize {
			partSize = remaining
		}
		chunkPath := filepath.Join(transportDir, string(rune('0'+part)))
		chunk, err := os.Create(chunkPath)
		if err != nil {
			t.Fatal(err)
		}
		chunkHash := sha256.New()
		for written := int64(0); written < partSize; {
			amount := int64(len(buffer))
			if partSize-written < amount {
				amount = partSize - written
			}
			piece := buffer[:int(amount)]
			if _, err := chunk.Write(piece); err != nil {
				t.Fatal(err)
			}
			_, _ = chunkHash.Write(piece)
			_, _ = fullHash.Write(piece)
			written += amount
		}
		if err := chunk.Close(); err != nil {
			t.Fatal(err)
		}
		relative, err := filepath.Rel(root, chunkPath)
		if err != nil {
			t.Fatal(err)
		}
		manifestFile.Chunks = append(manifestFile.Chunks, transport.Chunk{
			Path:   filepath.ToSlash(relative),
			Size:   partSize,
			SHA256: hex.EncodeToString(chunkHash.Sum(nil)),
		})
		remaining -= partSize
	}
	manifestFile.SHA256 = hex.EncodeToString(fullHash.Sum(nil))
	manifest := transport.Manifest{Version: transport.Version, Files: []transport.File{manifestFile}}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(transport.ManifestPath)), encoded, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := RestoreLargeSnapshotFiles(root); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(root, "ios", "Libraries", "libLarge.a")
	contents, err := os.Open(restored)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, contents); err != nil {
		t.Fatal(err)
	}
	if err := contents.Close(); err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(digest.Sum(nil)); got != manifestFile.SHA256 {
		t.Fatalf("restored digest = %s, want %s", got, manifestFile.SHA256)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(transport.Root))); !os.IsNotExist(err) {
		t.Fatal("transport directory was not removed after restoration")
	}
}

func TestRestoreLargeSnapshotFilesRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(transport.Root)), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := transport.Manifest{
		Version: transport.Version,
		Files: []transport.File{{
			Path:   "../outside",
			Size:   transport.LargeFileThreshold + 1,
			SHA256: string(make([]byte, 64)),
			Chunks: []transport.Chunk{{Path: transport.ChunkRoot + "/x/0", Size: 1, SHA256: string(make([]byte, 64))}},
		}},
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(transport.ManifestPath)), encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RestoreLargeSnapshotFiles(root); err == nil {
		t.Fatal("RestoreLargeSnapshotFiles accepted path traversal")
	}
}
