package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/MobAI-App/ios-builder/internal/config"
	"github.com/MobAI-App/ios-builder/internal/projectdetect"
	"github.com/MobAI-App/ios-builder/internal/registry"
)

// projectFacts is everything `central register` records about a project, read
// from the project itself rather than from hand-edited builder.json fields.
type projectFacts struct {
	AppPath   string
	IOSPath   string
	Framework string
	BundleID  string
	Notes     []string // one-line observations to print, e.g. a bundle id that was left unpinned
}

// readProjectFacts inspects the checkout at root. Values already present in
// builder.json (an explicit ios.appPath, ios.path) are kept; a disagreement
// with what the project contains is reported in Notes. bundleFlag, when set,
// overrides the detected bundle identifier.
func readProjectFacts(root string, cfg *config.Config, bundleFlag string) (*projectFacts, error) {
	layout, err := projectdetect.Resolve(root, cfg.IOS.AppPath)
	if err != nil {
		return nil, err
	}
	facts := &projectFacts{AppPath: layout.AppPath, IOSPath: layout.IOSPath}
	if cfg.IOS.AppPath != "" {
		facts.AppPath = cfg.IOS.AppPath
	}
	if cfg.IOS.Path != "" {
		if cfg.IOS.Path != layout.IOSPath {
			facts.Notes = append(facts.Notes, fmt.Sprintf("builder.json ios.path is %q but the project's Xcode folder is %q; using builder.json (edit or remove ios.path to follow the project)", cfg.IOS.Path, layout.IOSPath))
		}
		facts.IOSPath = cfg.IOS.Path
	}
	if facts.AppPath == "." {
		facts.AppPath = ""
	}
	if facts.IOSPath == "." {
		facts.IOSPath = ""
	}

	framework, err := projectdetect.DetectFramework(filepath.Join(root, filepath.FromSlash(layout.AppPath)))
	if err != nil {
		return nil, err
	}
	facts.Framework = framework

	switch {
	case bundleFlag != "":
		facts.BundleID = bundleFlag
	default:
		configuration := cfg.IOS.Configuration
		if configuration == "" {
			configuration = "Debug"
		}
		id, reason := projectdetect.BundleID(root, layout, configuration)
		if id != "" {
			facts.BundleID = id
			facts.Notes = append(facts.Notes, "bundle identifier read from the project: "+id)
		} else {
			facts.Notes = append(facts.Notes, "bundle identifier not pinned ("+reason+"); unsigned builds work, signing needs --bundle-id")
		}
	}
	if facts.BundleID != "" && !registry.BundleIDPattern.MatchString(facts.BundleID) {
		return nil, fmt.Errorf("bundle identifier %q is not valid", facts.BundleID)
	}
	return facts, nil
}

// describeNotes joins notes for printing, one per line.
func describeNotes(notes []string) string { return strings.Join(notes, "\n") }
