package main

import (
	"errors"
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

// registrationProblems compares what the project contains today with the
// registry entry recorded for it, and returns one line per difference. Each line
// states what the registry has, what the project has and how to fix it.
func registrationProblems(facts *projectFacts, cfg *config.Config, entry *registry.Project) []string {
	var problems []string
	differ := func(field, registered, current string) {
		if registered != current {
			problems = append(problems, fmt.Sprintf("registry %s is %q but the project has %q", field, registered, current))
		}
	}
	normalize := func(p string) string {
		if p == "." {
			return ""
		}
		return p
	}
	differ("repository", entry.Owner+"/"+entry.Repo, cfg.GitHub.Owner+"/"+cfg.GitHub.Repo)
	differ("app_path", normalize(entry.AppPath), facts.AppPath)
	differ("ios_path", normalize(entry.IOSPath), facts.IOSPath)
	differ("framework_hint", entry.FrameworkHint, facts.Framework)
	differ("scheme", entry.Scheme, cfg.IOS.Scheme)
	differ("snapshot_namespace", entry.SnapshotNamespace, cfg.SnapshotNamespace)
	configuration := cfg.IOS.Configuration
	if configuration == "" {
		configuration = "Debug"
	}
	differ("configuration", entry.Configuration, configuration)
	if entry.BundleID != "" && facts.BundleID != "" {
		differ("bundle_id", entry.BundleID, facts.BundleID)
	}
	return problems
}

// checkRegistration is the local part of `central doctor`: it re-reads the
// project and compares it with the local registry backup, so a stale
// registration is found now instead of when a build fails.
func checkRegistration(root string, cfg *config.Config, registryPath string) error {
	facts, err := readProjectFacts(root, cfg, "")
	if err != nil {
		return err
	}
	value, err := registry.LoadFile(registryPath)
	if err != nil {
		return fmt.Errorf("cannot read the local registry backup: %w; run `builder central register`", err)
	}
	entry, err := value.Resolve(cfg.ProjectID)
	if err != nil {
		return errors.New("this project is not in the local registry backup; run `builder central register`")
	}
	if problems := registrationProblems(facts, cfg, &entry); len(problems) > 0 {
		return fmt.Errorf("%s; run `builder central register` to update it", strings.Join(problems, "; "))
	}
	return nil
}
