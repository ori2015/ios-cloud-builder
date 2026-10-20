package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Screenshot capture runs the project's Maestro flow on an iPhone simulator and
// returns the PNGs it takes. App Store Connect needs one 6.9" iPhone size, which
// the Pro Max simulators produce natively (1320x2868), so the preferred device
// is the newest "Pro Max" model the runner ships.
const (
	screenshotFlowDir   = ".maestro"
	screenshotFlowFile  = "screenshots.yaml"
	maxScreenshotImages = 40
	// screenshotMediaDir, next to the flow, holds photos and videos added to the
	// simulator's library before the flow runs (for apps that import from Photos).
	screenshotMediaDir = "media"
	maxScreenshotMedia = 30
	faceIDMatchEvery   = 2 * time.Second
	// maestroDriverStartupTimeoutMs gives the XCTest driver five minutes to start.
	maestroDriverStartupTimeoutMs = "300000"
)

// simulatorTipsShown are com.apple.Preferences keys for keyboard introductions iOS shows once.
var simulatorTipsShown = []string{"DidShowContinuousPathIntroduction", "DidShowGestureKeyboardIntroduction"}

// isMaestroDriverStartupFailure reports whether Maestro failed before running any step
// because its iOS driver did not start; that is safe and worth one retry.
func isMaestroDriverStartupFailure(output []byte) bool {
	return bytes.Contains(output, []byte("iOS driver not ready in time")) ||
		bytes.Contains(output, []byte("IOSDriverTimeoutException"))
}

// runMaestroFlow runs the flow once more when the first attempt never got its driver up.
func runMaestroFlow(maestro executor, dir, udid, output, flow string) error {
	args := []string{"--no-ansi", "--device", udid, "test", "--test-output-dir", output, flow}
	out, err := maestro.runCollect(dir, "maestro", args...)
	if err != nil && isMaestroDriverStartupFailure(out) {
		fmt.Fprintln(maestro.log, "\nMaestro's iOS driver did not start; retrying the flow once.")
		_ = os.RemoveAll(output)
		_, err = maestro.runCollect(dir, "maestro", args...)
	}
	return err
}

var screenshotMediaExtensions = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".heic": true, ".mov": true, ".mp4": true}

// screenshotMedia lists the media files beside the flow, refusing anything that resolves outside the checkout.
func screenshotMedia(sourceRoot, flow string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(filepath.Dir(flow), screenshotMediaDir))
	if err != nil {
		return nil, nil
	}
	var files []string
	for _, entry := range entries {
		if !screenshotMediaExtensions[strings.ToLower(filepath.Ext(entry.Name()))] {
			continue
		}
		resolved, err := filepath.EvalSymlinks(filepath.Join(filepath.Dir(flow), screenshotMediaDir, entry.Name()))
		if err != nil {
			continue
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if !pathWithin(sourceRoot, resolved) {
			return nil, fmt.Errorf("screenshot media %s points outside the checkout", entry.Name())
		}
		files = append(files, resolved)
	}
	if len(files) > maxScreenshotMedia {
		return nil, fmt.Errorf("%d screenshot media files; the limit is %d", len(files), maxScreenshotMedia)
	}
	sort.Strings(files)
	return files, nil
}

// approveFaceIDWhileRunning enrolls Face ID on the simulator and keeps answering any Face ID
// prompt with a match until stop is called. A match with no prompt on screen does nothing, so
// apps that never ask are unaffected. This only drives the simulator; app code is unchanged.
func approveFaceIDWhileRunning(run executor, dir, udid string) (stop func()) {
	_ = run.run(dir, "xcrun", "simctl", "spawn", udid, "notifyutil", "-s", "com.apple.BiometricKit.enrollmentChanged", "1")
	_ = run.run(dir, "xcrun", "simctl", "spawn", udid, "notifyutil", "-p", "com.apple.BiometricKit.enrollmentChanged")
	ctx, cancel := context.WithCancel(run.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(faceIDMatchEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cmd := exec.CommandContext(ctx, "xcrun", "simctl", "spawn", udid, "notifyutil", "-p", "com.apple.BiometricKit_Sim.pearl.match")
				cmd.Dir = dir
				cmd.Env = run.env
				_ = cmd.Run() // quiet: one line every two seconds would bury the log
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

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
	return buildXcodeForSimulator(run, options, iosRoot, workspace, project, scheme, derivedData)
}

// simulatorEntitlements returns the project's entitlements file for the app target, read from
// the simulator build settings and required to lie inside the checkout ("" when there is none).
// The unsigned simulator build carries no entitlements, so CloudKit, HealthKit, App Groups and
// keychain groups would be missing at run time and such apps can exit on launch.
func simulatorEntitlements(run executor, iosRoot, sourceRoot, workspace, project, scheme, configuration string) string {
	if workspace == "" && project == "" {
		var err error
		if workspace, project, err = findXcodeContainer(iosRoot); err != nil {
			return ""
		}
	}
	if scheme == "" {
		scheme = "Runner"
	}
	args := []string{}
	if workspace != "" {
		args = append(args, "-workspace", workspace)
	} else {
		args = append(args, "-project", project)
	}
	args = append(args, "-scheme", scheme, "-configuration", configuration, "-sdk", "iphonesimulator", "-showBuildSettings")
	output, err := run.capture(iosRoot, "xcodebuild", args...)
	if err != nil {
		return ""
	}
	path, ok := entitlementsFileFromBuildSettings(output, sourceRoot)
	if !ok {
		return ""
	}
	return path
}

// resolvedSimulatorEntitlements writes a copy of the entitlements with the signing variables
// a local ad hoc simulator signature has no value for replaced (team prefix empty).
func resolvedSimulatorEntitlements(source, bundleID, dir string) (string, error) {
	data, err := readBoundedRegularFile(source, maxEntitlementsRequestBytes)
	if err != nil {
		return "", err
	}
	text := strings.NewReplacer(
		"$(AppIdentifierPrefix)", "", "$(TeamIdentifierPrefix)", "",
		"$(PRODUCT_BUNDLE_IDENTIFIER)", bundleID, "${PRODUCT_BUNDLE_IDENTIFIER}", bundleID,
	).Replace(string(data))
	path := filepath.Join(dir, "simulator.entitlements")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		return "", err
	}
	return path, nil
}

func buildXcodeForSimulator(run executor, options *BuildOptions, iosRoot, workspace, project, scheme, derivedData string) (string, error) {
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
func captureScreenshots(run executor, sourceRoot, appPath, entitlements, flow, outputPath string) error {
	media, err := screenshotMedia(sourceRoot, flow)
	if err != nil {
		return err
	}
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
	// One-time system tips (the QuickPath "slide to type" sheet) would cover the app in a screenshot;
	// mark them as already shown on this throwaway simulator.
	for _, key := range simulatorTipsShown {
		_ = run.run(dir, "xcrun", "simctl", "spawn", sim.UDID, "defaults", "write", "com.apple.Preferences", key, "-bool", "true")
	}
	// Apple's marketing status bar: 9:41, full battery and signal.
	_ = run.run(dir, "xcrun", "simctl", "status_bar", sim.UDID, "override",
		"--time", "9:41", "--dataNetwork", "wifi", "--wifiMode", "active", "--wifiBars", "3",
		"--cellularMode", "active", "--cellularBars", "4", "--batteryState", "charged", "--batteryLevel", "100")
	// Apple silicon simulators need at least an ad hoc signature; the build ran without signing.
	_ = run.run(dir, "/usr/bin/codesign", "--force", "--deep", "--sign", "-", appPath)
	if entitlements != "" {
		bundleID, _, _ := readAppMetadata(filepath.Join(appPath, "Info.plist"))
		if resolved, err := resolvedSimulatorEntitlements(entitlements, bundleID, filepath.Dir(outputPath)); err == nil {
			// Re-sign the app itself (frameworks keep the signature above) with its entitlements.
			_ = run.run(dir, "/usr/bin/codesign", "--force", "--sign", "-", "--entitlements", resolved, appPath)
			defer func() { _ = os.Remove(resolved) }()
		}
	}
	started := time.Now()
	if err := run.run(dir, "xcrun", "simctl", "install", sim.UDID, appPath); err != nil {
		return err
	}
	if len(media) > 0 {
		if err := run.run(dir, "xcrun", append([]string{"simctl", "addmedia", sim.UDID}, media...)...); err != nil {
			return err
		}
	}
	shots := filepath.Join(filepath.Dir(outputPath), "screenshots")
	_ = os.RemoveAll(shots)
	if err := os.Mkdir(shots, 0700); err != nil {
		return fmt.Errorf("prepare screenshot output")
	}
	defer func() { _ = os.RemoveAll(shots) }()
	// Maestro writes takeScreenshot files relative to its working directory.
	maestro := run
	maestro.env = append(append([]string{}, run.env...), "MAESTRO_CLI_NO_ANALYTICS=1", "MAESTRO_CLI_ANALYSIS_NOTIFICATION_DISABLED=true",
		// The XCTest driver can take minutes to come up on a freshly booted hosted runner.
		"MAESTRO_DRIVER_STARTUP_TIMEOUT="+maestroDriverStartupTimeoutMs)
	// Maestro 2.x writes takeScreenshot files to <test-output-dir>/takeScreenshot/; its own
	// per-step debug screenshots go elsewhere in that folder and are not returned.
	output := filepath.Join(shots, "maestro-output")
	stopFaceID := approveFaceIDWhileRunning(run, dir, sim.UDID)
	flowErr := runMaestroFlow(maestro, shots, sim.UDID, output, flow)
	stopFaceID()
	if home, err := os.UserHomeDir(); err == nil {
		appendCrashReports(run.log, filepath.Join(home, "Library", "Logs", "DiagnosticReports"),
			filepath.Base(strings.TrimSuffix(appPath, ".app")), started)
	}
	images := collectFlowScreenshots(shots)
	if len(images) == 0 && flowErr == nil {
		return fmt.Errorf("the screenshot flow took no screenshots (use takeScreenshot)")
	}
	if len(images) > maxScreenshotImages {
		return fmt.Errorf("the screenshot flow took %d screenshots; the limit is %d", len(images), maxScreenshotImages)
	}
	if flowErr != nil {
		bundleID, _, _ := readAppMetadata(filepath.Join(appPath, "Info.plist"))
		appendSimulatorAppLog(run, dir, sim.UDID, filepath.Base(strings.TrimSuffix(appPath, ".app")), bundleID)
		appendConsoleLaunch(run, dir, sim.UDID, bundleID)
		// A failed required step fails the run. What was captured, plus Maestro's own failure
		// screenshots (prefixed debug_), still travels encrypted so the caller can see where it stopped.
		images = append(images, collectMaestroFailureScreenshots(shots)...)
		if len(images) == 0 {
			return fmt.Errorf("screenshot flow failed: %w", flowErr)
		}
		fmt.Fprintf(run.log, "\nScreenshot flow failed (%v); returning %d images for diagnosis.\n", flowErr, len(images))
	}
	// Pack only the collected screenshots, flat, under their own names.
	packed := filepath.Join(shots, "packed")
	if err := os.Mkdir(packed, 0700); err != nil {
		return fmt.Errorf("prepare screenshot archive")
	}
	for _, image := range images {
		data, err := os.ReadFile(image)
		if err != nil {
			return fmt.Errorf("read screenshot: %w", err)
		}
		name := filepath.Base(image)
		if filepath.Base(filepath.Dir(image)) == "screenshots" {
			name = "debug_" + name
		}
		if err := os.WriteFile(filepath.Join(packed, name), data, 0600); err != nil {
			return fmt.Errorf("stage screenshot: %w", err)
		}
	}
	fmt.Fprintf(run.log, "\nCollected %d screenshots.\n", len(images))
	_ = os.Remove(outputPath)
	if err := run.run(packed, "/usr/bin/ditto", "-c", "-k", "--norsrc", ".", outputPath); err != nil {
		return err
	}
	if flowErr != nil {
		return fmt.Errorf("screenshot flow failed: %w", flowErr)
	}
	return nil
}

// collectMaestroFailureScreenshots returns the screenshots Maestro takes itself when a step
// fails (<test-output-dir>/screenshots/), at most a few, newest last.
func collectMaestroFailureScreenshots(shots string) []string {
	var images []string
	_ = filepath.Walk(shots, func(path string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() && strings.EqualFold(filepath.Ext(path), ".png") &&
			filepath.Base(filepath.Dir(path)) == "screenshots" {
			images = append(images, path)
		}
		return nil
	})
	sort.Strings(images)
	if len(images) > 3 {
		images = images[len(images)-3:]
	}
	return images
}

// collectFlowScreenshots returns the PNGs written by takeScreenshot: those in a
// takeScreenshot folder (Maestro 2.x), or directly in the working folder (older Maestro).
func collectFlowScreenshots(shots string) []string {
	var images []string
	_ = filepath.Walk(shots, func(path string, info os.FileInfo, err error) error {
		if err != nil || !info.Mode().IsRegular() || !strings.EqualFold(filepath.Ext(path), ".png") {
			return nil
		}
		if dir := filepath.Dir(path); filepath.Base(dir) == "takeScreenshot" || dir == shots {
			images = append(images, path)
		}
		return nil
	})
	sort.Strings(images)
	return images
}

// appendCrashReports copies the head of any crash report the simulator wrote for the app
// during the flow into the private (encrypted) log, so a launch crash is diagnosable.
func appendCrashReports(log io.Writer, dir, executable string, since time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	shown := 0
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.ModTime().Before(since) ||
			!strings.HasPrefix(entry.Name(), executable) || shown == 3 {
			continue
		}
		data, err := readBoundedRegularFile(filepath.Join(dir, entry.Name()), 2<<20)
		if err != nil {
			continue
		}
		lines := strings.SplitN(string(data), "\n", 121)
		if len(lines) > 120 {
			lines = lines[:120]
		}
		fmt.Fprintf(log, "\n--- crash report %s (first %d lines) ---\n%s\n", entry.Name(), len(lines), strings.Join(lines, "\n"))
		shown++
	}
}

// simulatorAppLogPredicate selects the app's own messages and what the system said about it, which
// explains a launch that ends without a crash report (killed, failed signature or entitlement checks).
func simulatorAppLogPredicate(executable, bundleID string) string {
	exe := strings.ReplaceAll(executable, `"`, ``)
	id := strings.ReplaceAll(bundleID, `"`, ``)
	// XCTest's icon-hierarchy chatter mentions the app hundreds of times a second and buries the launch.
	predicate := fmt.Sprintf(`(process == %[1]q OR eventMessage CONTAINS %[1]q`, exe)
	if id != "" {
		// Launch refusals (launchd, dyld, code signing) name the bundle, not the process.
		predicate += fmt.Sprintf(` OR eventMessage CONTAINS[c] %q`, id)
	}
	return predicate + `) AND NOT (subsystem == "com.apple.dt.xctest") AND NOT (eventMessage CONTAINS "PlugInKit") AND NOT (category == "Library")`
}

// appendSimulatorAppLog adds the last minutes of the simulator log for the app to the private log.
func appendSimulatorAppLog(run executor, dir, udid, executable, bundleID string) {
	// Not run.capture: that copies the whole output into the log; only the tail is wanted.
	cmd := exec.CommandContext(run.ctx, "xcrun", "simctl", "spawn", udid, "log", "show", "--last", "6m", "--style", "compact",
		"--predicate", simulatorAppLogPredicate(executable, bundleID))
	cmd.Dir = dir
	cmd.Env = run.env
	out, err := cmd.Output()
	if err != nil && len(out) == 0 {
		fmt.Fprintf(run.log, "\nSimulator log unavailable: %v\n", err)
		return
	}
	lines := strings.Split(string(out), "\n")
	if len(lines) > 400 {
		lines = lines[len(lines)-400:]
	}
	fmt.Fprintf(run.log, "\n--- simulator log for %s (last %d lines) ---\n%s\n", executable, len(lines), strings.Join(lines, "\n"))
}

// consoleLaunchWindow is how long a diagnostic launch's stdout and stderr are recorded.
const consoleLaunchWindow = 15 * time.Second

// appendConsoleLaunch launches the app once more with its console attached and records what it
// prints in the first seconds: a Swift fatal error, a dyld failure or an uncaught exception shows
// here even when the app dies before writing a crash report.
func appendConsoleLaunch(run executor, dir, udid, bundleID string) {
	if bundleID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(run.ctx, consoleLaunchWindow)
	defer cancel()
	cmd := exec.CommandContext(ctx, "xcrun", "simctl", "launch", "--console-pty", "--terminate-running-process", udid, bundleID)
	cmd.Dir = dir
	cmd.Env = run.env
	out, err := cmd.CombinedOutput()
	if len(out) > 32<<10 {
		out = out[len(out)-32<<10:]
	}
	fmt.Fprintf(run.log, "\n--- console of a diagnostic launch of %s (%s, %v) ---\n%s\n", bundleID, consoleLaunchWindow, err, out)
	pid := launchedPID(out, bundleID)
	if pid == "" {
		return
	}
	// What the process and the system said about this exact launch, including why it ended.
	logCmd := exec.CommandContext(run.ctx, "xcrun", "simctl", "spawn", udid, "log", "show", "--last", "2m", "--style", "compact",
		"--predicate", fmt.Sprintf(`processID == %s OR eventMessage CONTAINS %q`, pid, "["+pid+"]")+` OR eventMessage CONTAINS "`+pid+`"`)
	logCmd.Dir = dir
	logCmd.Env = run.env
	logOut, _ := logCmd.Output()
	lines := strings.Split(string(logOut), "\n")
	if len(lines) > 300 {
		lines = lines[len(lines)-300:]
	}
	fmt.Fprintf(run.log, "\n--- simulator log for pid %s (last %d lines) ---\n%s\n", pid, len(lines), strings.Join(lines, "\n"))
}

// launchedPID reads the "<bundle id>: <pid>" line simctl launch prints first.
func launchedPID(output []byte, bundleID string) string {
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, bundleID+": "); ok {
			if _, err := strconv.Atoi(rest); err == nil {
				return rest
			}
		}
	}
	return ""
}
