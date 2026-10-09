package runner

import "testing"

func TestReadToolVersions(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  ToolVersions
	}{
		{"nothing pinned", map[string]string{"README.md": "x"}, ToolVersions{}},
		{"flutter-version file", map[string]string{".flutter-version": "3.24.5\n"}, ToolVersions{Flutter: "3.24.5"}},
		{"fvmrc", map[string]string{".fvmrc": `{"flutter":"3.22.1"}`}, ToolVersions{Flutter: "3.22.1"}},
		{"fvm config", map[string]string{".fvm/fvm_config.json": `{"flutterSdkVersion":"3.19.6"}`}, ToolVersions{Flutter: "3.19.6"}},
		{"fvm channel pin is ignored", map[string]string{".fvmrc": `{"flutter":"stable"}`}, ToolVersions{}},
		{"fvm version at channel is ignored", map[string]string{".fvmrc": `{"flutter":"3.22.1@beta"}`}, ToolVersions{}},
		{"flutter-version beats fvm", map[string]string{".flutter-version": "3.24.5", ".fvmrc": `{"flutter":"3.22.1"}`}, ToolVersions{Flutter: "3.24.5"}},
		{"garbage flutter pin falls through", map[string]string{".flutter-version": "latest; rm -rf /", ".fvmrc": `{"flutter":"3.22.1"}`}, ToolVersions{Flutter: "3.22.1"}},
		{"pubspec.lock is not a pin", map[string]string{"pubspec.lock": "sdks:\n  dart: \">=3.0.0 <4.0.0\"\n  flutter: \">=3.0.0\"\n"}, ToolVersions{}},
		{"nvmrc with v prefix", map[string]string{".nvmrc": "v20.11.1\n"}, ToolVersions{Node: "20.11.1"}},
		{"nvmrc major", map[string]string{".nvmrc": "18"}, ToolVersions{Node: "18"}},
		{"nvmrc lts", map[string]string{".nvmrc": "lts/*"}, ToolVersions{Node: "lts/*"}},
		{"node-version", map[string]string{".node-version": "22.3.0"}, ToolVersions{Node: "22.3.0"}},
		{"nvmrc alias is not guessed", map[string]string{".nvmrc": "iojs"}, ToolVersions{}},
		{"engines range", map[string]string{"package.json": `{"engines":{"node":">=18 <21"}}`}, ToolVersions{Node: "20"}},
		{"engines caret", map[string]string{"package.json": `{"engines":{"node":"^18.17.0"}}`}, ToolVersions{Node: "18"}},
		{"engines or", map[string]string{"package.json": `{"engines":{"node":"16 || 18"}}`}, ToolVersions{Node: "18"}},
		{"engines unparsed", map[string]string{"package.json": `{"engines":{"node":"16 - 18"}}`}, ToolVersions{}},
		{"xcode", map[string]string{".xcode-version": "16.2\n"}, ToolVersions{Xcode: "16.2"}},
		{"xcode garbage", map[string]string{".xcode-version": "16.2 && echo"}, ToolVersions{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for name, contents := range tc.files {
				writeFile(t, root, name, contents)
			}
			if got := ReadToolVersions(root, root); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestReadToolVersionsPrefersAppFolderOverRoot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".flutter-version", "3.10.0")
	writeFile(t, root, ".xcode-version", "15.4")
	writeFile(t, root, "apps/mobile/.flutter-version", "3.24.5")
	got := ReadToolVersions(root, root+"/apps/mobile")
	if got.Flutter != "3.24.5" || got.Xcode != "15.4" {
		t.Fatalf("got %+v", got)
	}
}

func TestNodeMajorFor(t *testing.T) {
	for rng, want := range map[string]string{
		"":              "",
		">=16":          "20",
		">= 18":         "20",
		"<20":           "18",
		"20.x":          "20",
		"~20.5.0":       "20",
		">=14 <=16":     "16",
		"*":             "20",
		"<14":           "",
		"not a version": "",
	} {
		if got := nodeMajorFor(rng); got != want {
			t.Errorf("nodeMajorFor(%q) = %q, want %q", rng, got, want)
		}
	}
}
