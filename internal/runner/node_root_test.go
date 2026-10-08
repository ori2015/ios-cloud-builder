package runner

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNodeProjectRoot(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"package.json", "web/package.json", "plain/readme.txt"} {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name, iosPath, want string
	}{
		{"standard layout", "ios", root},
		{"checkout root", ".", root},
		{"app in a subdirectory", "web/ios", filepath.Join(root, "web")},
		{"subdirectory without package.json", "plain/ios", root},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nodeProjectRoot(root, tt.iosPath); got != tt.want {
				t.Fatalf("nodeProjectRoot = %q, want %q", got, tt.want)
			}
		})
	}
}
