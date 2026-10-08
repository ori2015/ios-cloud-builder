package runner

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFlutterProjectRoot(t *testing.T) {
	root := t.TempDir()
	write := func(rel string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("name: x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pubspec.yaml")
	write("app/pubspec.yaml")
	write("other/ios/Runner.txt")

	tests := []struct {
		name    string
		iosPath string
		want    string
	}{
		{"standard layout", "ios", root},
		{"app in a subdirectory", "app/ios", filepath.Join(root, "app")},
		{"subdirectory without a pubspec", "other/ios", root},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := flutterProjectRoot(root, filepath.Join(root, tt.iosPath)); got != tt.want {
				t.Fatalf("flutterProjectRoot = %q, want %q", got, tt.want)
			}
		})
	}
}
