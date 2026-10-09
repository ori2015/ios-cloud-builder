package runner

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"howett.net/plist"
)

// installFakeTools puts shell stand-ins for external tools first on PATH. Each
// records its arguments (with the working directory) in a shared log, and runs
// the optional body. It lets tests assert the exact command sequence the
// pipeline runs without the real toolchain. POSIX only.
func installFakeTools(t *testing.T, tools map[string]string) (logPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake tools are POSIX shell scripts")
	}
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	for name, body := range tools {
		script := "#!/bin/sh\necho \"" + name + " $* @$(basename \"$PWD\")\" >> '" + logPath + "'\n" + body + "\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

func readCalls(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// writeSampleIPA writes a minimal valid unsigned IPA for a fake build tool to copy.
func writeSampleIPA(t *testing.T, path, bundleID string) {
	t.Helper()
	info, err := plist.Marshal(map[string]any{
		"CFBundleIdentifier": bundleID, "CFBundleExecutable": "Sample", "CFBundleShortVersionString": "1.0",
	}, plist.XMLFormat)
	if err != nil {
		t.Fatal(err)
	}
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, data := range map[string][]byte{
		"Payload/Sample.app/Info.plist": info,
		"Payload/Sample.app/Sample":     {0xCF, 0xFA, 0xED, 0xFE, 0, 0, 0, 0},
	} {
		file, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buffer.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}
