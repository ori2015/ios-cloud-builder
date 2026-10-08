package runner

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectFrameworkCapacitorWithoutIonic(t *testing.T) {
	root := t.TempDir()
	pkg := `{"dependencies":{"@capacitor/ios":"^7.0.0","react":"^18.0.0"}}`
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(pkg), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := DetectFramework(root, FrameworkAuto); got != FrameworkNative {
		t.Fatalf("without a Capacitor config the project stays native, got %q", got)
	}
	if err := os.WriteFile(filepath.Join(root, "capacitor.config.ts"), []byte("export default {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := DetectFramework(root, FrameworkAuto)
	if err != nil || got != FrameworkIonic {
		t.Fatalf("DetectFramework = %q, %v; want %q", got, err, FrameworkIonic)
	}
}
