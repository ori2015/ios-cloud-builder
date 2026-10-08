// Package ipacheck inspects an unsigned IPA the way a downstream signer or
// installer would, so a build is only reported as working when the archive it
// produced is structurally a valid iOS application.
package ipacheck

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"howett.net/plist"
)

const maxPlistBytes = 4 * 1024 * 1024

// Info is the identity read from the application inside an IPA.
type Info struct {
	AppName          string // directory name of the .app bundle, e.g. "Example.app"
	BundleID         string
	Executable       string
	MarketingVersion string
	MinimumOSVersion string
}

// Inspect validates the IPA at path and returns the application identity.
func Inspect(ipaPath string) (*Info, error) {
	reader, err := zip.OpenReader(ipaPath)
	if err != nil {
		return nil, fmt.Errorf("open IPA: %w", err)
	}
	defer func() { _ = reader.Close() }()
	return InspectZip(&reader.Reader)
}

// InspectBytes validates an in-memory IPA.
func InspectBytes(data []byte) (*Info, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("open IPA: %w", err)
	}
	return InspectZip(reader)
}

// InspectZip validates an already opened IPA archive.
func InspectZip(reader *zip.Reader) (*Info, error) {
	files := make(map[string]*zip.File, len(reader.File))
	apps := make(map[string]bool)
	for _, file := range reader.File {
		name := file.Name
		if strings.HasPrefix(name, "/") || strings.Contains(name, `\`) || hasDotDot(name) {
			return nil, fmt.Errorf("IPA contains unsafe path %q", name)
		}
		files[name] = file
		parts := strings.Split(name, "/")
		if len(parts) >= 2 && parts[0] == "Payload" && strings.HasSuffix(parts[1], ".app") {
			apps[parts[1]] = true
		}
	}
	switch len(apps) {
	case 0:
		return nil, errors.New("IPA has no Payload/<name>.app bundle")
	case 1:
	default:
		return nil, errors.New("IPA contains more than one application bundle in Payload")
	}
	var appName string
	for name := range apps {
		appName = name
	}
	root := "Payload/" + appName + "/"

	infoFile, ok := files[root+"Info.plist"]
	if !ok {
		return nil, fmt.Errorf("%s has no Info.plist", appName)
	}
	data, err := readAll(infoFile, maxPlistBytes)
	if err != nil {
		return nil, fmt.Errorf("read Info.plist: %w", err)
	}
	var raw struct {
		BundleID   string `plist:"CFBundleIdentifier"`
		Executable string `plist:"CFBundleExecutable"`
		Version    string `plist:"CFBundleShortVersionString"`
		MinimumOS  string `plist:"MinimumOSVersion"`
	}
	if _, err := plist.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse Info.plist: %w", err)
	}
	if raw.BundleID == "" {
		return nil, errors.New("the Info.plist has no CFBundleIdentifier")
	}
	if raw.Executable == "" || path.Base(raw.Executable) != raw.Executable {
		return nil, errors.New("the Info.plist has no valid CFBundleExecutable")
	}
	binary, ok := files[root+raw.Executable]
	if !ok {
		return nil, fmt.Errorf("executable %q named by Info.plist is missing from %s", raw.Executable, appName)
	}
	head, err := readHead(binary, 4)
	if err != nil {
		return nil, fmt.Errorf("read executable: %w", err)
	}
	if !isMachO(head) {
		return nil, fmt.Errorf("executable %q is not a Mach-O binary", raw.Executable)
	}
	return &Info{
		AppName: appName, BundleID: raw.BundleID, Executable: raw.Executable,
		MarketingVersion: raw.Version, MinimumOSVersion: raw.MinimumOS,
	}, nil
}

func hasDotDot(name string) bool {
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

func readAll(file *zip.File, limit int64) ([]byte, error) {
	if file.UncompressedSize64 > uint64(limit) {
		return nil, errors.New("file is too large")
	}
	handle, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	return io.ReadAll(io.LimitReader(handle, limit))
}

func readHead(file *zip.File, n int) ([]byte, error) {
	handle, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	head := make([]byte, n)
	if _, err := io.ReadFull(handle, head); err != nil {
		return nil, err
	}
	return head, nil
}

// isMachO accepts thin 32/64-bit images in either byte order and fat binaries.
func isMachO(head []byte) bool {
	if len(head) < 4 {
		return false
	}
	switch [4]byte{head[0], head[1], head[2], head[3]} {
	case [4]byte{0xFE, 0xED, 0xFA, 0xCE}, [4]byte{0xFE, 0xED, 0xFA, 0xCF},
		[4]byte{0xCE, 0xFA, 0xED, 0xFE}, [4]byte{0xCF, 0xFA, 0xED, 0xFE},
		[4]byte{0xCA, 0xFE, 0xBA, 0xBE}, [4]byte{0xBE, 0xBA, 0xFE, 0xCA}:
		return true
	}
	return false
}
