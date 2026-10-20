package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPickScreenshotSimulatorPrefersNewestProMax(t *testing.T) {
	list := []byte(`{"devices":{
	 "com.apple.CoreSimulator.SimRuntime.iOS-18-6":[{"udid":"A","name":"iPhone 16 Pro Max","isAvailable":true}],
	 "com.apple.CoreSimulator.SimRuntime.iOS-26-2":[
	  {"udid":"B","name":"iPhone 17","isAvailable":true},
	  {"udid":"C","name":"iPhone 16 Pro Max","isAvailable":true},
	  {"udid":"D","name":"iPhone 17 Pro Max","isAvailable":true},
	  {"udid":"E","name":"iPhone 18 Pro Max","isAvailable":false}]}}`)
	got, err := pickScreenshotSimulator(list)
	if err != nil || got.UDID != "D" || got.Runtime != "26.2" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestPickScreenshotSimulatorFallsBackToAnyIPhone(t *testing.T) {
	list := []byte(`{"devices":{"com.apple.CoreSimulator.SimRuntime.iOS-26-0":[{"udid":"B","name":"iPhone 17","isAvailable":true}]}}`)
	got, err := pickScreenshotSimulator(list)
	if err != nil || got.UDID != "B" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestScreenshotFlowPrefersAppRootAndStaysInCheckout(t *testing.T) {
	source := t.TempDir()
	app := filepath.Join(source, "app")
	for _, dir := range []string{filepath.Join(app, ".maestro"), filepath.Join(source, ".maestro")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "screenshots.yaml"), []byte("appId: x\n---\n- launchApp\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	source, _ = filepath.EvalSymlinks(source)
	got, err := screenshotFlow(source, filepath.Join(source, "app"))
	if err != nil || got != filepath.Join(source, "app", ".maestro", "screenshots.yaml") {
		t.Fatalf("got %q, %v", got, err)
	}
	outside := t.TempDir()
	_ = os.WriteFile(filepath.Join(outside, "flow.yaml"), []byte("x"), 0600)
	_ = os.Remove(filepath.Join(source, "app", ".maestro", "screenshots.yaml"))
	_ = os.Symlink(filepath.Join(outside, "flow.yaml"), filepath.Join(source, "app", ".maestro", "screenshots.yaml"))
	if _, err := screenshotFlow(source, filepath.Join(source, "app")); err == nil {
		t.Fatal("a flow outside the checkout was accepted")
	}
}

func TestScreenshotFlowMissing(t *testing.T) {
	source := t.TempDir()
	if _, err := screenshotFlow(source, source); err == nil {
		t.Fatal("missing flow accepted")
	}
}

func TestCollectFlowScreenshotsSkipsMaestroDebugImages(t *testing.T) {
	shots := t.TempDir()
	write := func(rel string) {
		path := filepath.Join(shots, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("png"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("maestro-output/takeScreenshot/02_map.png")
	write("maestro-output/takeScreenshot/01_home.png")
	write("maestro-output/screenshots/screenshot-step-3.png")
	write("maestro-output/commands.json")
	write("legacy.png")
	got := collectFlowScreenshots(shots)
	var names []string
	for _, p := range got {
		names = append(names, filepath.Base(p))
	}
	if len(names) != 3 || names[0] != "legacy.png" || names[1] != "01_home.png" || names[2] != "02_map.png" {
		t.Fatalf("got %v", names)
	}
}

func TestScreenshotMediaListsOnlyMediaInsideCheckout(t *testing.T) {
	source := t.TempDir()
	source, _ = filepath.EvalSymlinks(source)
	media := filepath.Join(source, ".maestro", "media")
	if err := os.MkdirAll(media, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"b.jpg", "a.PNG", "notes.txt", "clip.mov"} {
		_ = os.WriteFile(filepath.Join(media, name), []byte("x"), 0600)
	}
	flow := filepath.Join(source, ".maestro", "screenshots.yaml")
	got, err := screenshotMedia(source, flow)
	if err != nil || len(got) != 3 || filepath.Base(got[0]) != "a.PNG" {
		t.Fatalf("got %v, %v", got, err)
	}
	outside := filepath.Join(t.TempDir(), "x.jpg")
	_ = os.WriteFile(outside, []byte("x"), 0600)
	_ = os.Symlink(outside, filepath.Join(media, "z.jpg"))
	if _, err := screenshotMedia(source, flow); err == nil {
		t.Fatal("media outside the checkout accepted")
	}
}

func TestScreenshotMediaMissingDirectoryIsFine(t *testing.T) {
	source := t.TempDir()
	got, err := screenshotMedia(source, filepath.Join(source, ".maestro", "screenshots.yaml"))
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestCollectMaestroFailureScreenshotsKeepsNewestThree(t *testing.T) {
	shots := t.TempDir()
	for _, rel := range []string{
		"maestro-output/screenshots/screenshot-1.png", "maestro-output/screenshots/screenshot-2.png",
		"maestro-output/screenshots/screenshot-3.png", "maestro-output/screenshots/screenshot-4.png",
		"maestro-output/takeScreenshot/01_home.png",
	} {
		path := filepath.Join(shots, rel)
		_ = os.MkdirAll(filepath.Dir(path), 0700)
		_ = os.WriteFile(path, []byte("png"), 0600)
	}
	got := collectMaestroFailureScreenshots(shots)
	if len(got) != 3 || filepath.Base(got[0]) != "screenshot-2.png" || filepath.Base(got[2]) != "screenshot-4.png" {
		t.Fatalf("got %v", got)
	}
}

func TestResolvedSimulatorEntitlementsFillsSigningVariables(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "App.entitlements")
	_ = os.WriteFile(source, []byte(`<plist><dict><key>keychain-access-groups</key><array><string>$(AppIdentifierPrefix)$(PRODUCT_BUNDLE_IDENTIFIER)</string></array><key>com.apple.developer.icloud-container-identifiers</key><array><string>iCloud.com.example.app</string></array></dict></plist>`), 0600)
	out, err := resolvedSimulatorEntitlements(source, "com.example.app", dir)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(out)
	if !strings.Contains(string(data), "<string>com.example.app</string>") || strings.Contains(string(data), "$(") ||
		!strings.Contains(string(data), "iCloud.com.example.app") {
		t.Fatalf("got %s", data)
	}
}

func TestAppendCrashReportsShowsOnlyThisAppsNewReports(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "Library", "Logs", "DiagnosticReports")
	_ = os.MkdirAll(dir, 0700)
	since := time.Now().Add(-time.Minute)
	_ = os.WriteFile(filepath.Join(dir, "FlowSip-2026-10-20-120000.ips"), []byte("Exception Type: EXC_CRASH\nline2"), 0600)
	_ = os.WriteFile(filepath.Join(dir, "Other-2026-10-20-120000.ips"), []byte("other app"), 0600)
	old := filepath.Join(dir, "FlowSip-2026-10-19-120000.ips")
	_ = os.WriteFile(old, []byte("old crash"), 0600)
	_ = os.Chtimes(old, since.Add(-time.Hour), since.Add(-time.Hour))
	var log strings.Builder
	appendCrashReports(&log, "FlowSip", since)
	got := log.String()
	if !strings.Contains(got, "EXC_CRASH") || strings.Contains(got, "other app") || strings.Contains(got, "old crash") {
		t.Fatalf("got %q", got)
	}
}
