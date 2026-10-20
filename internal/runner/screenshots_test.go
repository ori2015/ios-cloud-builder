package runner

import (
	"os"
	"path/filepath"
	"testing"
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
