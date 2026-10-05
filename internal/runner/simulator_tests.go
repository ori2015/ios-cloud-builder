package runner

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type simulatorChoice struct {
	UDID    string
	Name    string
	Runtime string // e.g. "26.2"
}

var runtimeKeyPattern = regexp.MustCompile(`SimRuntime\.iOS-(\d+)-(\d+)$`)

// pickSimulator chooses an available iPhone on the newest installed iOS runtime from
// `xcrun simctl list devices available -j` output.
func pickSimulator(listJSON []byte) (simulatorChoice, error) {
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
	type candidate struct {
		major, minor int
		choice       simulatorChoice
	}
	var all []candidate
	for key, devices := range parsed.Devices {
		m := runtimeKeyPattern.FindStringSubmatch(key)
		if m == nil {
			continue
		}
		major, _ := strconv.Atoi(m[1])
		minor, _ := strconv.Atoi(m[2])
		for _, d := range devices {
			if d.IsAvailable && strings.HasPrefix(d.Name, "iPhone") && d.UDID != "" {
				all = append(all, candidate{major, minor, simulatorChoice{UDID: d.UDID, Name: d.Name, Runtime: fmt.Sprintf("%d.%d", major, minor)}})
			}
		}
	}
	if len(all) == 0 {
		return simulatorChoice{}, fmt.Errorf("no available iPhone simulator found")
	}
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.major != b.major {
			return a.major > b.major
		}
		if a.minor != b.minor {
			return a.minor > b.minor
		}
		return a.choice.Name < b.choice.Name
	})
	return all[0].choice, nil
}

// runSimulatorTests runs the project's unit tests on the newest installed simulator. The
// deployment target is lowered to that runtime so projects targeting a newer OS than the
// runner's Xcode ships can still execute their tests; this affects only this test build.
func runSimulatorTests(run executor, iosRoot, workspace, project, scheme, derivedData string) error {
	list, err := run.capture(iosRoot, "xcrun", "simctl", "list", "devices", "available", "-j")
	if err != nil {
		return fmt.Errorf("list simulators: %w", err)
	}
	sim, err := pickSimulator(list)
	if err != nil {
		return err
	}
	fmt.Fprintf(run.log, "Running tests on %s (iOS %s)\n", sim.Name, sim.Runtime)
	// xcodebuild only lists iOS simulators once CoreSimulator has them booted/registered on a fresh runner;
	// boot first (errors ignored: it may already be booted) and wait until it is ready.
	_ = run.run(iosRoot, "xcrun", "simctl", "boot", sim.UDID)
	_ = run.run(iosRoot, "xcrun", "simctl", "bootstatus", sim.UDID, "-b")
	args := []string{}
	if workspace != "" {
		args = append(args, "-workspace", workspace)
	} else {
		args = append(args, "-project", project)
	}
	args = append(args,
		"-scheme", scheme,
		"-destination", "platform=iOS Simulator,id="+sim.UDID,
		"-derivedDataPath", derivedData,
		"IPHONEOS_DEPLOYMENT_TARGET="+sim.Runtime,
		"CODE_SIGNING_ALLOWED=NO",
		"CODE_SIGNING_REQUIRED=NO",
		"COMPILER_INDEX_STORE_ENABLE=NO",
		"test",
	)
	err = run.run(iosRoot, "xcodebuild", args...)
	if err == nil {
		return nil
	}
	// Diagnostics (private log only): what Xcode thinks it can run on.
	showArgs := filterOutDestination(append(append([]string{}, args[:len(args)-1]...), "-showdestinations"))
	_ = run.run(iosRoot, "xcodebuild", showArgs...)
	_ = run.run(iosRoot, "xcrun", "simctl", "list", "runtimes")
	// Some runner images list iOS simulators in simctl but not in xcodebuild. Compiling the app and its
	// test bundle for the simulator SDK still verifies the build, so fall back to that and say so clearly.
	fmt.Fprintln(run.log, "\nNo runnable simulator destination: falling back to build-for-testing (compile only, tests are NOT executed).")
	fallback := filterDestinationArgs(args[:len(args)-1])
	fallback = append(fallback, "-destination", "generic/platform=iOS Simulator", "-derivedDataPath", derivedData,
		"IPHONEOS_DEPLOYMENT_TARGET="+sim.Runtime, "CODE_SIGNING_ALLOWED=NO", "CODE_SIGNING_REQUIRED=NO",
		"COMPILER_INDEX_STORE_ENABLE=NO", "build-for-testing")
	if ferr := run.run(iosRoot, "xcodebuild", fallback...); ferr != nil {
		return err
	}
	return fmt.Errorf("tests compiled but could not be executed (no runnable simulator destination)")
}

// filterDestinationArgs keeps only the container and scheme arguments.
func filterDestinationArgs(args []string) []string {
	out := []string{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-project", "-workspace", "-scheme":
			if i+1 < len(args) {
				out = append(out, args[i], args[i+1])
				i++
			}
		}
	}
	return out
}

// filterOutDestination drops the -destination pair and build-setting overrides for -showdestinations.
func filterOutDestination(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-destination" || args[i] == "-derivedDataPath":
			i++
		case strings.Contains(args[i], "="):
		default:
			out = append(out, args[i])
		}
	}
	return out
}
