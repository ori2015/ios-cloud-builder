// Package transport defines the private snapshot transport format shared by
// the CLI and the trusted public runner.
package transport

const (
	// Root is reserved inside temporary snapshot commits. It never appears in
	// the restored source tree.
	Root = ".ios-builder-transport"

	ManifestPath = Root + "/manifest.json"
	ChunkRoot    = Root + "/chunks"

	Version = 1

	// Split files before GitHub's 100 MB hard blob limit. Chunks remain below
	// GitHub's 50 MB recommendation as well.
	LargeFileThreshold int64 = 45 * 1024 * 1024
	ChunkSize          int64 = 40 * 1024 * 1024
)

type Manifest struct {
	Version int    `json:"version"`
	Files   []File `json:"files"`
}

type File struct {
	Path       string  `json:"path"`
	Executable bool    `json:"executable"`
	Size       int64   `json:"size"`
	SHA256     string  `json:"sha256"`
	Chunks     []Chunk `json:"chunks"`
}

type Chunk struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
