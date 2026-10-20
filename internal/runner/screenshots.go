package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Screenshot capture runs the project's Maestro flow on an iPhone simulator and
// returns the PNGs it takes. App Store Connect needs one 6.9" iPhone size, which
// the Pro Max simulators produce natively (1320x2868), so the preferred device
// is the newest "Pro Max" model the runner ships.
const (
	screenshotFlowDir   = ".maestro"
	screenshotFlowFile  = "screenshots.yaml"
	maxScreenshotImages = 40
)

// screenshotFlow returns the Maestro flow for the app: <app>/.maestro/screenshots.yaml,
// falling back to the repository root. The flow is project content, like the
// project's build scripts, and runs with the same scrubbed environment.
func screenshotFlow(sourceRoot, appRoot string) (string, error) {
	for _, root := range []string{appRoot, sourceRoot} {
		path := filepath.Join(root, screenshotFlowDir, screenshotFlowFile)
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			continue
		}
		if !pathWithin(sourceRoot, resolved) {
			return "", fmt.Errorf("the screenshot flow points outside the checkout")
		}
		return resolved, nil
	}
	return "", fmt.Errorf("no screenshot flow: add %s/%s next to the app (a Maestro flow that uses takeScreenshot)", screenshotFlowDir, screenshotFlowFile)
}

// pickScreenshotSimulator prefers the newest-runtime iPhone "Pro Max", then any iPhone.
func pickScreenshotSimulator(listJSON []byte) (simulatorChoice, error) {
	var parsed struct {
		Devices map[string][]struct {
			UDID        string `json:"udid"`
			Name        string `json:"name"`
			IsAvailable bool   `json:"isAvailable"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(listJSON, &parsed); err != nil {
		return simulatorChoice{}, fmt.Errorf("parse simulator list: %w", err)
	}
	keys := make([]string, 0, len(parsed.Devices))
	for key := range parsed.Devices {
		if runtimeKeyPattern.MatchString(key) {
			keys = append(keys, key)
		}
	}
	// Newest runtime first: compare major, then minor, numerically.
	sort.Slice(keys, func(i, j int) bool {
		a, b := runtimeKeyPattern.FindStringSubmatch(keys[i]), runtimeKeyPattern.FindStringSubmatch(keys[j])
		if len(a[1]) != len(b[1]) || a[1] != b[1] {
			return atoi(a[1]) > atoi(b[1])
		}
		return atoi(a[2]) > atoi(b[2])
	})
	for _, key := range keys {
		m := runtimeKeyPattern.FindStringSubmatch(key)
		var names []simulatorChoice
		for _, d := range parsed.Devices[key] {
			if d.IsAvailable && strings.HasPrefix(d.Name, "iPhone") && strings.HasSuffix(d.Name, "Pro Max") && d.UDID != "" {
				names = append(names, simulatorChoice{UDID: d.UDID, Name: d.Name, Runtime: m[1] + "." + m[2]})
			}
		}
		if len(names) > 0 {
			// "iPhone 17 Pro Max" sorts after "iPhone 16 Pro Max": take the last one.
			sort.Slice(names, func(i, j int) bool { return names[i].Name < names[j].Name })
			return names[len(names)-1], nil
		}
	}
	return pickSimulator(listJSON)
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

// buildForSimulator compiles the app for the simulator SDK and returns the .app path.
func buildForSimulator(run executor, options *BuildOptions, appRoot, iosRoot, workspace, project, scheme, derivedData string) (string, error) {
	if options.Framework == FrameworkFlutter {
		// Flutter only builds debug for the simulator; the debug banner is the app's own choice.
		if err := run.run(appRoot, "flutter", "build", "ios", "--simulator", "--debug"); err != nil {
			return "", err
		}
		return findApp(filepath.Join(appRoot, "build", "ios", "iphonesimulator"))
	}
	args := []string{}
	if workspace != "" {
		args = append(args, "-workspace", workspace)
	} else {
		args = append(args, "-project", project)
	}
	args = append(args,
		"-scheme", scheme,
		"-configuration", options.Configuration,
		"-sdk", "iphonesimulator",
		"-destination", "generic/platform=iOS Simulator",
		"-derivedDataPath", derivedData,
		"CODE_SIGNING_ALLOWED=NO",
		"CODE_SIGNING_REQUIRED=NO",
		"COMPILER_INDEX_STORE_ENABLE=NO",
		"build",
	)
	if err := run.run(iosRoot, "xcodebuild", args...); err != nil {
		return "", err
	}
	return findApp(filepath.Join(derivedData, "Build", "Products", options.Configuration+"-iphonesimulator"))
}

// captureScreenshots installs the simulator app, runs the Maestro flow and packs the PNGs
// it produced into a zip at outputPath (the encrypted "IPA" slot of the artifact).
func captureScreenshots(run executor, appPath, flow, outputPath string) error {
	list, err := run.capture(filepath.Dir(appPath), "xcrun", "simctl", "list", "devices", "available", "-j")
	if err != nil {
		return fmt.Errorf("list simulators: %w", err)
	}
	sim, err := pickScreenshotSimulator(list)
	if err != nil {
		return err
	}
	fmt.Fprintf(run.log, "Capturing screenshots on %s (iOS %s)\n", sim.Name, sim.Runtime)
	dir := filepath.Dir(appPath)
	_ = run.run(dir, "xcrun", "simctl", "boot", sim.UDID)
	if err := run.run(dir, "xcrun", "simctl", "bootstatus", sim.UDID, "-b"); err != nil {
		return err
	}
	// Apple's marketing status bar: 9:41, full battery and signal.
	_ = run.run(dir, "xcrun", "simctl", "status_bar", sim.UDID, "override",
		"--time", "9:41", "--dataNetwork", "wifi", "--wifiMode", "active", "--wifiBars", "3",
		"--cellularMode", "active", "--cellularBars", "4", "--batteryState", "charged", "--batteryLevel", "100")
	if err := run.run(dir, "xcrun", "simctl", "install", sim.UDID, appPath); err != nil {
		return err
	}
	shots := filepath.Join(filepath.Dir(outputPath), "screenshots")
	_ = os.RemoveAll(shots)
	if err := os.Mkdir(shots, 0700); err != nil {
		return fmt.Errorf("prepare screenshot output")
	}
	defer func() { _ = os.RemoveAll(shots) }()
	// Maestro writes takeScreenshot files relative to its working directory.
	maestro := run
	maestro.env = append(append([]string{}, run.env...), "MAESTRO_CLI_NO_ANALYTICS=1", "MAESTRO_CLI_ANALYSIS_NOTIFICATION_DISABLED=true")
	flowErr := maestro.run(shots, "maestro", "--no-ansi", "--device", sim.UDID, "test", flow)
	var images []string
	_ = filepath.Walk(shots, func(path string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() && strings.EqualFold(filepath.Ext(path), ".png") {
			images = append(images, path)
		}
		return nil
	})
	if len(images) == 0 {
		if flowErr != nil {
			return flowErr
		}
		return fmt.Errorf("the screenshot flow took no screenshots (use takeScreenshot)")
	}
	if len(images) > maxScreenshotImages {
		return fmt.Errorf("the screenshot flow took %d screenshots; the limit is %d", len(images), maxScreenshotImages)
	}
	if flowErr != nil {
		// Keep what was captured; the log says where the flow stopped.
		fmt.Fprintf(run.log, "\nScreenshot flow stopped early (%v); returning the %d screenshots taken.\n", flowErr, len(images))
	}
	_ = os.Remove(outputPath)
	return run.run(shots, "/usr/bin/ditto", "-c", "-k", "--norsrc", ".", outputPath)
}
