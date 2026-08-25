// Package registry implements the secret-backed opaque project registry used
// by the public central builder. Plaintext registries belong only in GitHub
// Actions secrets and a mode-0600 local operator backup.
package registry

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	SchemaVersion = 1
	SecretName    = "PROJECT_REGISTRY"
	maxProjects   = 1000
	maxJSONBytes  = 48 * 1024
)

var (
	ProjectIDPattern         = regexp.MustCompile(`^p_[0-9a-f]{32}$`)
	SnapshotNamespacePattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	ownerPattern             = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
	repoPattern              = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
	schemePattern            = regexp.MustCompile(`^$|^[A-Za-z0-9][A-Za-z0-9 ._+()-]{0,127}$`)
)

type Registry struct {
	Version  int                `json:"version"`
	Revision uint64             `json:"revision"`
	Projects map[string]Project `json:"projects"`
}

type Project struct {
	Owner             string `json:"owner"`
	Repo              string `json:"repo"`
	IOSPath           string `json:"ios_path,omitempty"`
	Scheme            string `json:"scheme,omitempty"`
	Configuration     string `json:"configuration"`
	FrameworkHint     string `json:"framework_hint"`
	SnapshotNamespace string `json:"snapshot_namespace"`
}

func New() *Registry { return &Registry{Version: SchemaVersion, Projects: make(map[string]Project)} }

func NewProjectID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate opaque project ID: %w", err)
	}
	return "p_" + hex.EncodeToString(value), nil
}

func NewSnapshotNamespace() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate private snapshot namespace: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func Parse(data []byte) (*Registry, error) {
	if len(data) == 0 || len(data) > maxJSONBytes {
		return nil, errors.New("project registry is empty or oversized")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var value Registry
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("parse project registry: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("project registry contains trailing JSON")
	}
	if err := value.Validate(); err != nil {
		return nil, err
	}
	return &value, nil
}

func (r *Registry) Validate() error {
	if r == nil || r.Version != SchemaVersion || r.Revision == 0 || len(r.Projects) == 0 || len(r.Projects) > maxProjects {
		return errors.New("invalid project registry metadata")
	}
	for id, project := range r.Projects {
		if !ProjectIDPattern.MatchString(id) {
			return errors.New("project registry contains an invalid project ID")
		}
		if err := project.Validate(); err != nil {
			return fmt.Errorf("invalid project registry entry %s: %w", id, err)
		}
	}
	return nil
}

func (p *Project) Validate() error {
	if p == nil {
		return errors.New("missing project")
	}
	if !ownerPattern.MatchString(p.Owner) || strings.Contains(p.Owner, "--") {
		return errors.New("invalid owner")
	}
	if !repoPattern.MatchString(p.Repo) || p.Repo == "." || p.Repo == ".." || strings.HasSuffix(strings.ToLower(p.Repo), ".git") {
		return errors.New("invalid repository")
	}
	iosPath := p.IOSPath
	if iosPath == "" {
		iosPath = "."
	}
	if filepath.IsAbs(iosPath) || filepath.Clean(iosPath) != iosPath || iosPath == ".." || strings.HasPrefix(iosPath, "../") || strings.ContainsAny(iosPath, `\`+"\r\n\x00") {
		return errors.New("invalid iOS path")
	}
	if !schemePattern.MatchString(p.Scheme) {
		return errors.New("invalid scheme")
	}
	if p.Configuration != "Debug" && p.Configuration != "Release" {
		return errors.New("invalid configuration")
	}
	switch p.FrameworkHint {
	case "auto", "native", "flutter", "react-native", "expo", "kmp", "cordova", "ionic":
	default:
		return errors.New("invalid framework hint")
	}
	if !SnapshotNamespacePattern.MatchString(p.SnapshotNamespace) {
		return errors.New("invalid snapshot namespace")
	}
	return nil
}

func (r *Registry) Resolve(id string) (Project, error) {
	if !ProjectIDPattern.MatchString(id) {
		return Project{}, errors.New("invalid project ID")
	}
	project, ok := r.Projects[id]
	if !ok {
		return Project{}, errors.New("unknown project ID")
	}
	return project, nil
}

func (r *Registry) Put(id string, project *Project) error {
	if !ProjectIDPattern.MatchString(id) {
		return errors.New("invalid project ID")
	}
	if err := project.Validate(); err != nil {
		return err
	}
	if r.Projects == nil {
		r.Projects = make(map[string]Project)
	}
	if len(r.Projects) >= maxProjects {
		if _, exists := r.Projects[id]; !exists {
			return errors.New("project registry is full")
		}
	}
	r.Version = SchemaVersion
	r.Revision++
	r.Projects[id] = *project
	return nil
}

func (r *Registry) Marshal() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	if len(data) > maxJSONBytes {
		return nil, errors.New("project registry exceeds the GitHub secret size limit")
	}
	return data, nil
}

func MaskValues(project *Project, snapshotRef string) []string {
	if project == nil {
		return nil
	}
	repositoryValues := []string{
		project.Owner + "/" + project.Repo,
		"https://github.com/" + project.Owner + "/" + project.Repo,
		"https://github.com/" + project.Owner + "/" + project.Repo + ".git",
		"git@github.com:" + project.Owner + "/" + project.Repo + ".git",
		project.Owner, project.Repo,
	}
	values := append([]string{}, repositoryValues...)
	for _, value := range repositoryValues {
		values = append(values, strings.ToLower(value))
	}
	values = append(values, project.SnapshotNamespace, snapshotRef, project.IOSPath, project.Scheme)
	seen := make(map[string]bool)
	filtered := values[:0]
	for _, value := range values {
		if value != "" && value != "." && !seen[value] {
			seen[value] = true
			filtered = append(filtered, value)
		}
	}
	sort.SliceStable(filtered, func(i, j int) bool { return len(filtered[i]) > len(filtered[j]) })
	return filtered
}

func LoadFile(path string) (*Registry, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return New(), nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("local project registry is not a regular mode-0600 file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

func SaveFile(path string, value []byte) error {
	if _, err := Parse(value); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	temporary := path + ".partial"
	_ = os.Remove(temporary)
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err := file.Write(value); err != nil {
		_ = file.Close()
		_ = os.Remove(temporary)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(temporary)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return os.Chmod(path, 0600)
}
