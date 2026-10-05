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
	return run.run(iosRoot, "xcodebuild", args...)
}
