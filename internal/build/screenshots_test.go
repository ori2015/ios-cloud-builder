package build

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func zipOf(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, data := range files {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write(data)
	}
	_ = w.Close()
	return buf.Bytes()
}

func TestSaveScreenshotsKeepsOnlyFlatPNGs(t *testing.T) {
	png := append(append([]byte{}, pngSignature...), 1, 2, 3)
	archive := zipOf(t, map[string][]byte{
		"./01_home.png":       png,
		"../../evil/02_x.png": png,
		"notes.txt":           []byte("ignored"),
		"sub/.hidden.png":     png,
	})
	dir := filepath.Join(t.TempDir(), "screenshots")
	got, err := saveScreenshots(archive, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || filepath.Base(got[0]) != "01_home.png" || filepath.Base(got[1]) != "02_x.png" {
		t.Fatalf("got %v", got)
	}
	for _, p := range got {
		if filepath.Dir(p) != dir {
			t.Fatalf("%s escaped %s", p, dir)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err == nil {
		t.Fatal("non-PNG file extracted")
	}
}

func TestSaveScreenshotsRejectsFakePNG(t *testing.T) {
	archive := zipOf(t, map[string][]byte{"a.png": []byte("not a png")})
	if _, err := saveScreenshots(archive, t.TempDir()); err == nil {
		t.Fatal("fake PNG accepted")
	}
}
