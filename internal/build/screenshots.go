package build

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const (
	maxScreenshotFiles = 40
	maxScreenshotBytes = 20 << 20
)

var pngSignature = []byte("\x89PNG\r\n\x1a\n")

// saveScreenshots extracts only flat-named PNG files from the decrypted archive.
// Directory parts are dropped and every file must carry the PNG signature, so the
// project cannot write anything else, or anywhere else, on the caller's machine.
func saveScreenshots(archive []byte, dir string) ([]string, error) {
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("read screenshot archive: %w", err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("prepare screenshot directory: %w", err)
	}
	var saved []string
	for _, file := range reader.File {
		name := path.Base(strings.ReplaceAll(file.Name, "\\", "/"))
		if file.FileInfo().IsDir() || !strings.EqualFold(path.Ext(name), ".png") || strings.HasPrefix(name, ".") {
			continue
		}
		if len(saved) == maxScreenshotFiles {
			return saved, errors.New("screenshot archive holds too many files")
		}
		if file.UncompressedSize64 > maxScreenshotBytes {
			return saved, fmt.Errorf("screenshot %s is too large", name)
		}
		rc, err := file.Open()
		if err != nil {
			return saved, err
		}
		data, err := io.ReadAll(io.LimitReader(rc, maxScreenshotBytes+1))
		rc.Close()
		if err != nil || len(data) > maxScreenshotBytes || !bytes.HasPrefix(data, pngSignature) {
			return saved, fmt.Errorf("screenshot %s is not a valid PNG", name)
		}
		target := filepath.Join(dir, name)
		if err := atomicWritePrivate(target, data); err != nil {
			return saved, err
		}
		saved = append(saved, target)
	}
	if len(saved) == 0 {
		return nil, errors.New("the screenshot archive holds no PNG files")
	}
	sort.Strings(saved)
	return saved, nil
}
