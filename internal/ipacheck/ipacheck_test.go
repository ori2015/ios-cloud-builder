package ipacheck

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"howett.net/plist"
)

type entry struct {
	name string
	data []byte
}

func buildIPA(t *testing.T, entries []entry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, e := range entries {
		file, err := writer.Create(e.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(e.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func infoPlist(t *testing.T, values map[string]any) []byte {
	t.Helper()
	data, err := plist.Marshal(values, plist.XMLFormat)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

var machO64 = []byte{0xCF, 0xFA, 0xED, 0xFE, 0, 0, 0, 0}

func validEntries(t *testing.T) []entry {
	return []entry{
		{"Payload/Example.app/Info.plist", infoPlist(t, map[string]any{
			"CFBundleIdentifier": "example.generic.app", "CFBundleExecutable": "Example",
			"CFBundleShortVersionString": "1.2.3", "MinimumOSVersion": "15.0",
		})},
		{"Payload/Example.app/Example", machO64},
	}
}

func TestInspectAcceptsUnsignedApplication(t *testing.T) {
	info, err := InspectBytes(buildIPA(t, validEntries(t)))
	if err != nil {
		t.Fatal(err)
	}
	if info.AppName != "Example.app" || info.BundleID != "example.generic.app" ||
		info.Executable != "Example" || info.MarketingVersion != "1.2.3" || info.MinimumOSVersion != "15.0" {
		t.Fatalf("unexpected info: %+v", info)
	}
}

func TestInspectFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "App.ipa")
	if err := os.WriteFile(path, buildIPA(t, validEntries(t)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(path); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(filepath.Join(t.TempDir(), "missing.ipa")); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestInspectRejectsMalformedArchives(t *testing.T) {
	good := validEntries(t)
	plistWith := func(values map[string]any) []byte { return infoPlist(t, values) }
	cases := map[string][]entry{
		"no payload":         {{"readme.txt", []byte("x")}},
		"no info plist":      {{"Payload/Example.app/Example", machO64}},
		"two applications":   append(append([]entry{}, good...), entry{"Payload/Other.app/Info.plist", good[0].data}),
		"missing executable": {good[0]},
		"script executable":  {good[0], {"Payload/Example.app/Example", []byte("#!/bin/sh\n")}},
		"no bundle id": {
			{"Payload/Example.app/Info.plist", plistWith(map[string]any{"CFBundleExecutable": "Example"})},
			good[1],
		},
		"no executable key": {
			{"Payload/Example.app/Info.plist", plistWith(map[string]any{"CFBundleIdentifier": "example.generic.app"})},
			good[1],
		},
		"executable path escape": {
			{"Payload/Example.app/Info.plist", plistWith(map[string]any{"CFBundleIdentifier": "a.b", "CFBundleExecutable": "../Example"})},
			good[1],
		},
		"corrupt plist":  {{"Payload/Example.app/Info.plist", []byte("not a plist")}, good[1]},
		"path traversal": append(append([]entry{}, good...), entry{"Payload/Example.app/../../evil", []byte("x")}),
	}
	for name, entries := range cases {
		t.Run(strings.ReplaceAll(name, " ", "_"), func(t *testing.T) {
			if _, err := InspectBytes(buildIPA(t, entries)); err == nil {
				t.Fatal("malformed IPA accepted")
			}
		})
	}
	if _, err := InspectBytes([]byte("not a zip")); err == nil {
		t.Fatal("non-zip accepted")
	}
}

func TestIsMachOMagicNumbers(t *testing.T) {
	for _, magic := range [][]byte{{0xFE, 0xED, 0xFA, 0xCF}, {0xCF, 0xFA, 0xED, 0xFE}, {0xCA, 0xFE, 0xBA, 0xBE}} {
		if !isMachO(magic) {
			t.Errorf("magic %x rejected", magic)
		}
	}
	if isMachO([]byte{0x7F, 'E', 'L', 'F'}) || isMachO([]byte{1, 2}) {
		t.Error("non-Mach-O accepted")
	}
}
