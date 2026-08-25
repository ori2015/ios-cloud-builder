package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/MobAI-App/ios-builder/internal/transport"
)

const (
	maxTransportManifestSize = 10 * 1024 * 1024
	maxTransportFiles        = 10000
	maxTransportChunks       = 100000
	maxRestoredBytes         = 100 * 1024 * 1024 * 1024
)

// RestoreLargeSnapshotFiles reconstructs files split by the CLI before a Git
// push. It runs only after the private checkout credential has been revoked.
func RestoreLargeSnapshotFiles(sourceRoot string) error {
	manifestPath := filepath.Join(sourceRoot, filepath.FromSlash(transport.ManifestPath))
	manifestInfo, err := os.Lstat(manifestPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !manifestInfo.Mode().IsRegular() || manifestInfo.Size() <= 0 || manifestInfo.Size() > maxTransportManifestSize {
		return fmt.Errorf("invalid snapshot transport manifest")
	}
	manifestFile, err := os.Open(manifestPath)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(io.LimitReader(manifestFile, maxTransportManifestSize+1))
	decoder.DisallowUnknownFields()
	var manifest transport.Manifest
	decodeErr := decoder.Decode(&manifest)
	if decodeErr == nil {
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			decodeErr = fmt.Errorf("manifest has trailing content")
		}
	}
	closeErr := manifestFile.Close()
	if decodeErr != nil {
		return fmt.Errorf("decode snapshot transport manifest: %w", decodeErr)
	}
	if closeErr != nil {
		return closeErr
	}
	if manifest.Version != transport.Version || len(manifest.Files) == 0 || len(manifest.Files) > maxTransportFiles {
		return fmt.Errorf("invalid snapshot transport manifest version or file count")
	}

	transportDir := filepath.Join(sourceRoot, filepath.FromSlash(transport.Root))
	transportInfo, err := os.Lstat(transportDir)
	if err != nil || !transportInfo.IsDir() || transportInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("invalid snapshot transport directory")
	}

	seenFiles := make(map[string]struct{}, len(manifest.Files))
	seenChunks := make(map[string]struct{})
	var totalBytes int64
	var totalChunks int
	for index := range manifest.Files {
		file := &manifest.Files[index]
		cleanPath, err := validateTransportPath(file.Path, false)
		if err != nil {
			return err
		}
		if _, exists := seenFiles[cleanPath]; exists {
			return fmt.Errorf("duplicate restored file path")
		}
		seenFiles[cleanPath] = struct{}{}
		if file.Size <= transport.LargeFileThreshold || file.Size > maxRestoredBytes || len(file.Chunks) == 0 {
			return fmt.Errorf("invalid restored file size or chunks")
		}
		if file.Size > maxRestoredBytes-totalBytes {
			return fmt.Errorf("snapshot transport exceeds restored size limit")
		}
		totalBytes += file.Size
		totalChunks += len(file.Chunks)
		if totalChunks > maxTransportChunks {
			return fmt.Errorf("snapshot transport exceeds chunk count limit")
		}
		if err := restoreTransportFile(sourceRoot, cleanPath, file, seenChunks); err != nil {
			return err
		}
	}
	return os.RemoveAll(transportDir)
}

func restoreTransportFile(sourceRoot, cleanPath string, file *transport.File, seenChunks map[string]struct{}) error {
	destination := filepath.Join(sourceRoot, filepath.FromSlash(cleanPath))
	if err := ensureNoSymlinkParents(sourceRoot, cleanPath); err != nil {
		return err
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		if err != nil {
			return err
		}
		return fmt.Errorf("restored file destination already exists")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".ios-builder-restore-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()

	fullHash := sha256.New()
	var restored int64
	for _, chunk := range file.Chunks {
		chunkPath, err := validateTransportPath(chunk.Path, true)
		if err != nil {
			return err
		}
		if _, exists := seenChunks[chunkPath]; exists {
			return fmt.Errorf("duplicate snapshot chunk")
		}
		seenChunks[chunkPath] = struct{}{}
		if chunk.Size <= 0 || chunk.Size > transport.ChunkSize || !validSHA256(chunk.SHA256) {
			return fmt.Errorf("invalid snapshot chunk metadata")
		}
		chunkFilePath := filepath.Join(sourceRoot, filepath.FromSlash(chunkPath))
		if err := ensureNoSymlinkParents(sourceRoot, chunkPath); err != nil {
			return err
		}
		chunkInfo, err := os.Lstat(chunkFilePath)
		if err != nil || !chunkInfo.Mode().IsRegular() || chunkInfo.Size() != chunk.Size {
			return fmt.Errorf("invalid snapshot chunk file")
		}
		chunkFile, err := os.Open(chunkFilePath)
		if err != nil {
			return err
		}
		chunkHash := sha256.New()
		copied, copyErr := io.Copy(io.MultiWriter(temporary, fullHash, chunkHash), chunkFile)
		closeErr := chunkFile.Close()
		if copyErr != nil || closeErr != nil || copied != chunk.Size {
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			return fmt.Errorf("snapshot chunk size mismatch")
		}
		if !strings.EqualFold(hex.EncodeToString(chunkHash.Sum(nil)), chunk.SHA256) {
			return fmt.Errorf("snapshot chunk digest mismatch")
		}
		restored += copied
	}
	if restored != file.Size || !validSHA256(file.SHA256) || !strings.EqualFold(hex.EncodeToString(fullHash.Sum(nil)), file.SHA256) {
		return fmt.Errorf("restored snapshot file digest mismatch")
	}
	mode := os.FileMode(0o644)
	if file.Executable {
		mode = 0o755
	}
	if err := temporary.Chmod(mode); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return err
	}
	committed = true
	return nil
}

func validateTransportPath(value string, chunk bool) (string, error) {
	if value == "" || strings.ContainsRune(value, '\x00') || strings.Contains(value, "\\") || !utf8.ValidString(value) {
		return "", fmt.Errorf("invalid snapshot transport path")
	}
	clean := path.Clean(value)
	if clean != value || clean == "." || strings.HasPrefix(clean, "/") || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("invalid snapshot transport path")
	}
	insideTransport := clean == transport.Root || strings.HasPrefix(clean, transport.Root+"/")
	if chunk {
		if !strings.HasPrefix(clean, transport.ChunkRoot+"/") {
			return "", fmt.Errorf("snapshot chunk is outside transport namespace")
		}
	} else if insideTransport {
		return "", fmt.Errorf("restored file uses transport namespace")
	}
	return clean, nil
}

func ensureNoSymlinkParents(sourceRoot, relative string) error {
	current := sourceRoot
	parts := strings.Split(path.Dir(relative), "/")
	if len(parts) == 1 && parts[0] == "." {
		return nil
	}
	for _, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("restored file parent is not a real directory")
		}
	}
	return nil
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
