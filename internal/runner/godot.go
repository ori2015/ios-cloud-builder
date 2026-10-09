package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/MobAI-App/ios-builder/internal/projectdetect"
)

const (
	// godotPlaceholderTeamID fills the iOS preset's required App Store Team ID
	// when the project leaves it empty. Nothing is signed, so any well-formed
	// value satisfies the exporter.
	godotPlaceholderTeamID = "AAAAAAAAAA"
	godotExportDir         = ".ios-export"
)

var godotTemplatesVersionRe = regexp.MustCompile(`^[0-9][0-9A-Za-z._-]*$`)

// generateGodotXcodeProject exports the project's iOS preset to an Xcode project
// with the editor in godotDir (installed by the workflow, outside the checkout)
// and returns the folder holding it. Proven on a macOS runner with Godot 4.4.1:
//
//	Godot --headless --path <app> --import
//	Godot --headless --path <app> --export-release "<preset>" <app>/.ios-export/App.xcodeproj
//
// The exporter creates App.xcodeproj plus the project files beside it; the
// shared unsigned xcodebuild stage then builds it. Two project settings are
// required for the export to succeed and are applied to the ephemeral checkout:
// ETC2/ASTC texture import (otherwise the export fails with no message) and
// "export project only" (otherwise Godot would try to archive and sign).
func generateGodotXcodeProject(run executor, appRoot, godotDir, privateHome string) (string, error) {
	if godotDir == "" {
		return "", fmt.Errorf("the Godot editor was not installed for this build (the workflow's Godot step provides --godot-dir)")
	}
	editor := filepath.Join(godotDir, "Godot.app", "Contents", "MacOS", "Godot")
	if !exists(editor) {
		return "", fmt.Errorf("the Godot editor is missing from the tools folder")
	}
	preset, ok := projectdetect.GodotIOSPreset(appRoot)
	if !ok {
		return "", fmt.Errorf("export_presets.cfg has no iOS preset; add one in the Godot editor (Project > Export > Add > iOS) and commit export_presets.cfg")
	}
	if err := linkGodotTemplates(godotDir, privateHome); err != nil {
		return "", err
	}
	if err := patchGodotProject(appRoot); err != nil {
		return "", err
	}
	exportDir := filepath.Join(appRoot, godotExportDir)
	if err := os.RemoveAll(exportDir); err != nil {
		return "", fmt.Errorf("prepare the export folder: %w", err)
	}
	if err := os.MkdirAll(exportDir, 0o700); err != nil {
		return "", fmt.Errorf("prepare the export folder: %w", err)
	}
	if err := run.run(appRoot, editor, "--headless", "--path", appRoot, "--import"); err != nil {
		return "", err
	}
	if err := run.run(appRoot, editor, "--headless", "--path", appRoot, "--export-release", preset, filepath.Join(exportDir, "App.xcodeproj")); err != nil {
		return "", err
	}
	return exportDir, nil
}

// linkGodotTemplates makes the installed export templates visible to the editor
// under the build's private HOME, where it looks for them.
func linkGodotTemplates(godotDir, privateHome string) error {
	templates := filepath.Join(godotDir, "templates")
	version, err := os.ReadFile(filepath.Join(templates, "version.txt"))
	if err != nil {
		return fmt.Errorf("the Godot export templates are missing from the tools folder")
	}
	name := strings.TrimSpace(string(version))
	if !godotTemplatesVersionRe.MatchString(name) {
		return fmt.Errorf("the Godot export templates report an invalid version")
	}
	parent := filepath.Join(privateHome, "Library", "Application Support", "Godot", "export_templates")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("prepare the Godot templates folder: %w", err)
	}
	return os.Symlink(templates, filepath.Join(parent, name))
}

// patchGodotProject applies the settings the headless iOS export needs.
func patchGodotProject(appRoot string) error {
	projectPath := filepath.Join(appRoot, "project.godot")
	project, err := os.ReadFile(projectPath)
	if err != nil {
		return fmt.Errorf("read project.godot: %w", err)
	}
	patched := setINIValue(string(project), "rendering", "textures/vram_compression/import_etc2_astc", "true")
	if err := os.WriteFile(projectPath, []byte(patched), 0o600); err != nil {
		return err
	}
	presetsPath := filepath.Join(appRoot, "export_presets.cfg")
	presets, err := os.ReadFile(presetsPath)
	if err != nil {
		return fmt.Errorf("read export_presets.cfg: %w", err)
	}
	updated, err := patchGodotPresets(string(presets))
	if err != nil {
		return err
	}
	return os.WriteFile(presetsPath, []byte(updated), 0o600)
}

// patchGodotPresets sets "export project only" and fills a blank Team ID in the
// options of the first iOS preset.
func patchGodotPresets(text string) (string, error) {
	sections := projectdetect.SplitINI(text)
	options := ""
	for _, section := range sections {
		if godotPresetHeaderRe.MatchString(section.Header) && projectdetect.INIValue(section.Body, "platform") == "iOS" {
			options = strings.TrimSuffix(section.Header, "]") + ".options]"
			break
		}
	}
	if options == "" {
		return "", fmt.Errorf("export_presets.cfg has no iOS preset")
	}
	section := strings.TrimSuffix(strings.TrimPrefix(options, "["), "]")
	text = setINIValue(text, section, "application/export_project_only", "true")
	if projectdetect.INIValue(sectionBody(text, section), "application/app_store_team_id") == "" {
		text = setINIValue(text, section, "application/app_store_team_id", `"`+godotPlaceholderTeamID+`"`)
	}
	return text, nil
}

var godotPresetHeaderRe = regexp.MustCompile(`^\[preset\.\d+\]$`)

func sectionBody(text, section string) string {
	for _, s := range projectdetect.SplitINI(text) {
		if s.Header == "["+section+"]" {
			return s.Body
		}
	}
	return ""
}

// setINIValue sets key=value inside [section], appending the key (or the whole
// section) when absent. The value is written as given, so quote strings yourself.
func setINIValue(text, section, key, value string) string {
	lines := strings.Split(text, "\n")
	header := "[" + section + "]"
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == header {
			start = i
			break
		}
	}
	entry := key + "=" + value
	if start < 0 {
		trimmed := strings.TrimRight(text, "\n")
		if trimmed != "" {
			trimmed += "\n\n"
		}
		return trimmed + header + "\n" + entry + "\n"
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			end = i
			break
		}
	}
	for i := start + 1; i < end; i++ {
		if k, _, ok := strings.Cut(strings.TrimSpace(lines[i]), "="); ok && strings.TrimSpace(k) == key {
			lines[i] = entry
			return strings.Join(lines, "\n")
		}
	}
	insert := start + 1
	out := append([]string{}, lines[:insert]...)
	out = append(out, entry)
	out = append(out, lines[insert:]...)
	return strings.Join(out, "\n")
}
