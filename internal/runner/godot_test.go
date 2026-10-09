package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleProject = "config_version=5\n\n[application]\n\nconfig/name=\"Generic\"\nconfig/features=PackedStringArray(\"4.4\")\n"

const samplePresets = "[preset.0]\n\nname=\"iOS\"\nplatform=\"iOS\"\nrunnable=true\n\n[preset.0.options]\n\napplication/bundle_identifier=\"example.generic.godot\"\napplication/app_store_team_id=\"\"\n"

func TestSetINIValue(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"adds the section", "a=1\n", "a=1\n\n[rendering]\nk=v\n"},
		{"adds the key", "[rendering]\nx=1\n\n[other]\ny=2\n", "[rendering]\nk=v\nx=1\n\n[other]\ny=2\n"},
		{"replaces the key", "[rendering]\nk=old\nx=1\n", "[rendering]\nk=v\nx=1\n"},
		{"leaves other sections' keys alone", "[other]\nk=keep\n[rendering]\nx=1\n", "[other]\nk=keep\n[rendering]\nk=v\nx=1\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := setINIValue(tc.in, "rendering", "k", "v"); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPatchGodotPresets(t *testing.T) {
	got, err := patchGodotPresets(samplePresets)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"application/export_project_only=true", `application/app_store_team_id="` + godotPlaceholderTeamID + `"`, `application/bundle_identifier="example.generic.godot"`} {
		if !strings.Contains(got, want) {
			t.Errorf("patched presets lack %q:\n%s", want, got)
		}
	}
	// an existing Team ID is the project's choice and is kept
	kept, err := patchGodotPresets(strings.Replace(samplePresets, `app_store_team_id=""`, `app_store_team_id="ZZZZZZZZZZ"`, 1))
	if err != nil || !strings.Contains(kept, "ZZZZZZZZZZ") || strings.Contains(kept, godotPlaceholderTeamID) {
		t.Fatalf("existing team id replaced: %v\n%s", err, kept)
	}
	if _, err := patchGodotPresets("[preset.0]\nname=\"Web\"\nplatform=\"Web\"\n"); err == nil {
		t.Fatal("presets without an iOS entry accepted")
	}
}

func fakeGodotDir(t *testing.T, calls, exportScript string) string {
	dir := t.TempDir()
	editor := filepath.Join(dir, "Godot.app", "Contents", "MacOS", "Godot")
	if err := os.MkdirAll(filepath.Dir(editor), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho \"godot $* @$(basename \"$PWD\")\" >> '" + calls + "'\n" + exportScript + "\n"
	if err := os.WriteFile(editor, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "templates/version.txt", "4.4.1.stable\n")
	return dir
}

func TestGodotExportsThenBuildsTheXcodeProject(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "project.godot", sampleProject)
	writeFile(t, root, "export_presets.cfg", samplePresets)
	writeFile(t, root, ".git/config", "[core]\n")
	calls := installFakeTools(t, map[string]string{
		"xcodebuild": "case \"$*\" in *-list*) echo '{\"project\":{\"schemes\":[\"App\"]}}';; esac",
	})
	// the fake editor records its calls in the same log and "exports" a project next to its output path ($6)
	godotDir := fakeGodotDir(t, calls, "case \"$*\" in *--export-release*) mkdir -p \"$6\";; esac")
	output := t.TempDir()
	options := &BuildOptions{
		Framework: FrameworkGodot, IOSPath: ".", Configuration: "Debug", SourceRoot: root, GodotDir: godotDir,
		LogPath: output + "/private-output/build.log", IPAPath: output + "/private-output/App.ipa",
	}
	_ = BuildUnsigned(t.Context(), options) // packaging needs macOS ditto
	got := strings.Join(readCalls(t, calls), "\n")
	real, err := filepath.EvalSymlinks(root) // the pipeline works on the resolved path (macOS temp dirs sit behind /var)
	if err != nil {
		t.Fatal(err)
	}
	order := []string{
		"--headless --path " + real + " --import",
		`--export-release iOS ` + filepath.Join(real, godotExportDir, "App.xcodeproj"),
		"xcodebuild -project App.xcodeproj -list -json",
		"-scheme App -configuration Debug -destination generic/platform=iOS",
	}
	last := -1
	for _, want := range order {
		i := strings.Index(got, want)
		if i < 0 || i < last {
			t.Fatalf("expected %q in order; calls:\n%s", want, got)
		}
		last = i
	}
	patched, _ := os.ReadFile(filepath.Join(root, "project.godot"))
	if !strings.Contains(string(patched), "textures/vram_compression/import_etc2_astc=true") {
		t.Errorf("ETC2/ASTC import not enabled:\n%s", patched)
	}
	// templates are reachable from the build's private home, not copied
	if _, err := os.Lstat(filepath.Join(godotDir, "templates", "version.txt")); err != nil {
		t.Error(err)
	}
}

func TestGodotNeedsTheEditor(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "project.godot", sampleProject)
	writeFile(t, root, "export_presets.cfg", samplePresets)
	writeFile(t, root, ".git/config", "[core]\n")
	output := t.TempDir() + "/private-output"
	options := &BuildOptions{
		Framework: FrameworkGodot, IOSPath: ".", Configuration: "Debug", SourceRoot: root,
		LogPath: output + "/build.log", IPAPath: output + "/App.ipa",
	}
	if err := BuildUnsigned(t.Context(), options); err == nil {
		t.Fatal("Godot build without the editor succeeded")
	}
	log, _ := os.ReadFile(options.LogPath)
	if !strings.Contains(string(log), "--godot-dir") {
		t.Fatalf("log does not say how the editor is provided:\n%s", log)
	}
	options.GodotDir = filepath.Join(root, "inside")
	if err := options.validate(); err == nil {
		t.Fatal("a Godot tools folder inside the checkout was accepted")
	}
}
