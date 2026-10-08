package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"howett.net/plist"
)

// Nested-bundle planning.
//
// The signing job receives an unsigned .app whose contents are project
// controlled. Before any credential is read, the app is enumerated into a
// signing plan: every piece of code that has to carry its own signature, in the
// order it must be signed (inside-out), together with the exact bundle
// identifier that its provisioning profile has to name.
//
// The plan is fail-closed. A bundle type or location that is not understood is
// an error, never a partially signed app. Nested bundle identifiers are never
// taken blindly: the registered (and already build-time verified) identifier of
// the main application is the only root of trust, and every nested identifier
// that gets a provisioning profile must be "<main>.<suffix>".

type bundleKind string

const (
	kindApp       bundleKind = "app"
	kindAppex     bundleKind = "app-extension"
	kindWatchApp  bundleKind = "watch-app"
	kindAppClip   bundleKind = "app-clip"
	kindXPC       bundleKind = "xpc-service"
	kindFramework bundleKind = "framework"
	kindDylib     bundleKind = "dylib"
)

const (
	platformIOS     = "ios"
	platformWatchOS = "watchos"

	maxPlanBundles      = 64
	maxPlanDepth        = 4
	maxPlanEntries      = maxDeployEntries
	maxBundleInfoBytes  = 4 * 1024 * 1024
	containerPlugIns    = "PlugIns"
	containerWatch      = "Watch"
	containerAppClips   = "AppClips"
	containerXPCService = "XPCServices"
)

// nestedSuffixPattern is what may follow "<main>." in a nested identifier:
// dot-separated, non-empty, DNS-like labels.
var nestedSuffixPattern = regexp.MustCompile(`^[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)*$`)

// signingTarget is one unit handed to codesign.
type signingTarget struct {
	Kind     bundleKind `json:"kind"`
	RelPath  string     `json:"path"` // slash separated, relative to the .app; "." for the app itself
	Path     string     `json:"-"`
	BundleID string     `json:"bundle_id,omitempty"`
	Platform string     `json:"platform"`
	Version  string     `json:"version,omitempty"`
	// ExtensionPoint is the NSExtensionPointIdentifier of an app extension.
	ExtensionPoint string `json:"extension_point,omitempty"`
	// NeedsProfile is true for bundles that are installable on their own and
	// therefore need an embedded provisioning profile and profile entitlements.
	NeedsProfile bool `json:"needs_profile"`
	// SetsBuildNumber is true for bundles whose CFBundleVersion must follow the
	// main application's build number.
	SetsBuildNumber bool   `json:"-"`
	InfoPath        string `json:"-"`
}

// signingPlan lists the targets inside-out: Steps[len-1] is the main app.
type signingPlan struct {
	MainBundleID string          `json:"main_bundle_id"`
	Steps        []signingTarget `json:"steps"`
	Warnings     []string        `json:"warnings,omitempty"`
}

func (p *signingPlan) main() *signingTarget { return &p.Steps[len(p.Steps)-1] }

// profileTargets returns the targets that need a provisioning profile.
func (p *signingPlan) profileTargets() []*signingTarget {
	var targets []*signingTarget
	for index := range p.Steps {
		if p.Steps[index].NeedsProfile {
			targets = append(targets, &p.Steps[index])
		}
	}
	return targets
}

type bundleInfo struct {
	BundleID       string         `plist:"CFBundleIdentifier"`
	ShortVersion   string         `plist:"CFBundleShortVersionString"`
	Executable     string         `plist:"CFBundleExecutable"`
	PlatformName   string         `plist:"DTPlatformName"`
	Extension      map[string]any `plist:"NSExtension"`
	WatchKitApp    bool           `plist:"WKWatchKitApp"`
	WatchApp       bool           `plist:"WKApplication"`
	CompanionAppID string         `plist:"WKCompanionAppBundleIdentifier"`
	AppClip        map[string]any `plist:"NSAppClip"`
}

func readBundleInfo(infoPath string) (*bundleInfo, error) {
	data, err := readBoundedRegularFile(infoPath, maxBundleInfoBytes)
	if err != nil {
		return nil, errors.New("embedded bundle has no readable Info.plist")
	}
	var info bundleInfo
	if _, err := plist.Unmarshal(data, &info); err != nil {
		return nil, errors.New("embedded bundle Info.plist is not a valid property list")
	}
	if strings.ContainsAny(info.BundleID+info.ShortVersion, "\r\n\x00") {
		return nil, errors.New("embedded bundle Info.plist contains control characters")
	}
	return &info, nil
}

// planSigning enumerates appPath. It never executes anything from the bundle.
func planSigning(appPath string) (*signingPlan, error) {
	info, err := os.Lstat(appPath)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("application bundle is not a plain directory")
	}
	mainInfo, err := readBundleInfo(filepath.Join(appPath, "Info.plist"))
	if err != nil {
		return nil, fmt.Errorf("application: %w", err)
	}
	if !bundleIDPattern.MatchString(mainInfo.BundleID) || mainInfo.ShortVersion == "" {
		return nil, errors.New("application bundle identifier or version is invalid")
	}
	if mainInfo.PlatformName != "" && mainInfo.PlatformName != "iphoneos" {
		return nil, errors.New("application was not built for iphoneos devices")
	}
	planner := &planner{appRoot: appPath, mainID: mainInfo.BundleID, ids: map[string]string{mainInfo.BundleID: "."}}
	root := signingTarget{
		Kind: kindApp, RelPath: ".", Path: appPath, BundleID: mainInfo.BundleID, Platform: platformIOS,
		Version: mainInfo.ShortVersion, NeedsProfile: true, SetsBuildNumber: true,
		InfoPath: filepath.Join(appPath, "Info.plist"),
	}
	steps, err := planner.walkBundle(&root, 0)
	if err != nil {
		return nil, err
	}
	return &signingPlan{MainBundleID: mainInfo.BundleID, Steps: steps, Warnings: planner.warnings}, nil
}

type planner struct {
	appRoot  string
	mainID   string
	ids      map[string]string // bundle id -> relative path, for duplicate detection
	bundles  int
	entries  int
	warnings []string
}

func (p *planner) warn(format string, args ...any) {
	if len(p.warnings) < 50 {
		p.warnings = append(p.warnings, fmt.Sprintf(format, args...))
	}
}

func rel(root, path string) string {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(relative)
}

// containerKinds says which container directories each bundle kind may hold and
// which bundle suffix they may contain.
var containerSuffix = map[string]string{
	containerPlugIns: ".appex", containerWatch: ".app", containerAppClips: ".app", containerXPCService: ".xpc",
}

func containerAllowed(parent bundleKind, container string) bool {
	switch container {
	case containerWatch, containerAppClips:
		return parent == kindApp
	case containerPlugIns:
		return parent == kindApp || parent == kindWatchApp || parent == kindAppClip
	case containerXPCService:
		return parent == kindApp || parent == kindAppex || parent == kindAppClip
	}
	return false
}

// walkBundle returns the targets inside bundle in signing order, ending with the
// bundle itself. Frameworks and dylibs come first, then nested app-like
// bundles (each fully inside-out), then the bundle.
func (p *planner) walkBundle(self *signingTarget, depth int) ([]signingTarget, error) {
	if depth > maxPlanDepth {
		return nil, errors.New("embedded bundles are nested too deeply")
	}
	p.bundles++
	if p.bundles > maxPlanBundles {
		return nil, errors.New("application embeds too many bundles")
	}
	var leaves []signingTarget
	var nested [][]signingTarget
	var visit func(dir string, atRoot bool) error
	visit = func(dir string, atRoot bool) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return errors.New("inspect application bundle")
		}
		for _, entry := range entries {
			p.entries++
			if p.entries > maxPlanEntries {
				return errors.New("application contains too many files")
			}
			name, path := entry.Name(), filepath.Join(dir, entry.Name())
			mode := entry.Type()
			if mode&os.ModeSymlink != 0 {
				return fmt.Errorf("symbolic link inside the application bundle: %s", rel(self.Path, path))
			}
			if !mode.IsDir() {
				if !mode.IsRegular() {
					return fmt.Errorf("special file inside the application bundle: %s", rel(self.Path, path))
				}
				if strings.HasSuffix(strings.ToLower(name), ".dylib") {
					leaves = append(leaves, signingTarget{Kind: kindDylib, RelPath: "", Path: path, Platform: self.Platform})
				}
				continue
			}
			lower := strings.ToLower(name)
			suffix := filepath.Ext(lower)
			switch {
			case suffix == ".framework":
				framework := signingTarget{Kind: kindFramework, Path: path, Platform: self.Platform}
				if info, err := readBundleInfo(filepath.Join(path, "Info.plist")); err == nil && bundleIDPattern.MatchString(info.BundleID) {
					framework.BundleID = info.BundleID
				}
				steps, err := p.walkBundle(&framework, depth+1)
				if err != nil {
					return err
				}
				leaves = append(leaves, steps...)
			case suffix == ".app" || suffix == ".appex" || suffix == ".xpc":
				return fmt.Errorf("unsupported embedded bundle location: %s", rel(self.Path, path))
			case containerSuffix[name] != "" || isContainerVariant(lower):
				if containerSuffix[name] == "" || !atRoot || !containerAllowed(self.Kind, name) {
					return fmt.Errorf("unsupported embedded bundle container: %s", rel(self.Path, path))
				}
				steps, err := p.walkContainer(self, path, name, depth)
				if err != nil {
					return err
				}
				nested = append(nested, steps)
			default:
				if err := visit(path, false); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := visit(self.Path, true); err != nil {
		return nil, err
	}
	for index := range leaves {
		leaves[index].RelPath = rel(p.appRoot, leaves[index].Path)
	}
	// Deepest leaves first, as before; ties by path for a stable plan.
	sort.SliceStable(leaves, func(i, j int) bool {
		di, dj := strings.Count(leaves[i].RelPath, "/"), strings.Count(leaves[j].RelPath, "/")
		if di != dj {
			return di > dj
		}
		return leaves[i].RelPath < leaves[j].RelPath
	})
	var steps []signingTarget
	steps = append(steps, leaves...)
	for _, group := range nested {
		steps = append(steps, group...)
	}
	return append(steps, *self), nil
}

func isContainerVariant(lower string) bool {
	for name := range containerSuffix {
		if strings.ToLower(name) == lower {
			return true
		}
	}
	return false
}

func (p *planner) walkContainer(parent *signingTarget, dir, container string, depth int) ([]signingTarget, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, errors.New("inspect embedded bundle container")
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("empty embedded bundle container: %s", container)
	}
	var out []signingTarget
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() || filepath.Ext(entry.Name()) != containerSuffix[container] {
			return nil, fmt.Errorf("unsupported entry in %s: %s", container, entry.Name())
		}
		target, err := p.describe(parent, path, container)
		if err != nil {
			return nil, err
		}
		steps, err := p.walkBundle(target, depth+1)
		if err != nil {
			return nil, err
		}
		out = append(out, steps...)
	}
	return out, nil
}

// describe reads the nested bundle's own Info.plist and applies the identifier
// rules for its kind.
func (p *planner) describe(parent *signingTarget, path, container string) (*signingTarget, error) {
	shown := rel(p.appRoot, path)
	info, err := readBundleInfo(filepath.Join(path, "Info.plist"))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", shown, err)
	}
	target := &signingTarget{
		Path: path, RelPath: shown, BundleID: info.BundleID, Platform: parent.Platform, Version: info.ShortVersion,
		NeedsProfile: true, SetsBuildNumber: true, InfoPath: filepath.Join(path, "Info.plist"),
	}
	switch container {
	case containerPlugIns:
		target.Kind = kindAppex
		point, _ := info.Extension["NSExtensionPointIdentifier"].(string)
		if point == "" || len(point) > 200 || strings.ContainsAny(point, "\r\n\x00") {
			return nil, fmt.Errorf("%s: app extension declares no NSExtensionPointIdentifier", shown)
		}
		target.ExtensionPoint = point
	case containerWatch:
		target.Kind, target.Platform = kindWatchApp, platformWatchOS
		if !info.WatchKitApp && !info.WatchApp {
			p.warn("%s: Info.plist declares neither WKWatchKitApp nor WKApplication", shown)
		}
		if info.CompanionAppID != "" && info.CompanionAppID != p.mainID {
			return nil, fmt.Errorf("%s: WKCompanionAppBundleIdentifier does not name the main application", shown)
		}
		if info.CompanionAppID == "" {
			p.warn("%s: no WKCompanionAppBundleIdentifier; App Store Connect may reject the Watch app", shown)
		}
	case containerAppClips:
		target.Kind = kindAppClip
		if info.AppClip == nil {
			p.warn("%s: Info.plist has no NSAppClip dictionary", shown)
		}
	case containerXPCService:
		target.Kind, target.NeedsProfile, target.SetsBuildNumber = kindXPC, false, false
	}
	if !wantsPlatform(info.PlatformName, target.Platform) {
		return nil, fmt.Errorf("%s: bundle was built for platform %q, not for device distribution", shown, info.PlatformName)
	}
	if err := p.checkNestedID(target.BundleID, shown); err != nil {
		return nil, err
	}
	if info.ShortVersion == "" {
		p.warn("%s: no CFBundleShortVersionString", shown)
	}
	p.ids[target.BundleID] = shown
	return target, nil
}

func wantsPlatform(declared, platform string) bool {
	switch declared {
	case "":
		return true // not every build writes DTPlatformName
	case "iphoneos":
		return platform == platformIOS
	case "watchos":
		return platform == platformWatchOS
	}
	return false
}

// checkNestedID enforces "<main>.<suffix>", rejects duplicates, and bounds the
// result with the same pattern the registry uses.
func (p *planner) checkNestedID(id, shown string) error {
	if !bundleIDPattern.MatchString(id) || !strings.HasPrefix(id, p.mainID+".") ||
		!nestedSuffixPattern.MatchString(strings.TrimPrefix(id, p.mainID+".")) {
		return fmt.Errorf("%s: bundle identifier is not prefixed by the registered application identifier", shown)
	}
	if other, duplicate := p.ids[id]; duplicate {
		return fmt.Errorf("%s: bundle identifier duplicates %s", shown, other)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Rendering for the credential-free dry run.

func (p *signingPlan) render(w io.Writer, profileType string) {
	_, _ = fmt.Fprintf(w, "Application bundle identifier: %s\n", p.MainBundleID)
	_, _ = fmt.Fprintf(w, "Signing order (inside-out), %d step(s):\n", len(p.Steps))
	profiles := 0
	for index, step := range p.Steps {
		line := fmt.Sprintf("  %2d. %-14s %s", index+1, step.Kind, step.RelPath)
		if step.BundleID != "" {
			line += "  id=" + step.BundleID
		}
		if step.Kind != kindDylib {
			line += "  platform=" + step.Platform
		}
		if step.ExtensionPoint != "" {
			line += "  extension-point=" + step.ExtensionPoint
		}
		if step.NeedsProfile {
			profiles++
			line += "  profile=" + profileType + " for exactly " + step.BundleID
		} else {
			line += "  profile=none (signed with the distribution identity only)"
		}
		_, _ = fmt.Fprintln(w, line)
	}
	_, _ = fmt.Fprintf(w, "Provisioning profiles required: %d\n", profiles)
	for _, warning := range p.Warnings {
		_, _ = fmt.Fprintf(w, "Warning: %s\n", warning)
	}
}

func (p *signingPlan) renderJSON(w io.Writer, profileType string) error {
	out := struct {
		*signingPlan
		ProfileType string `json:"profile_type"`
	}{p, profileType}
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(out)
}

// PlanSigningOptions drives the credential-free dry run.
type PlanSigningOptions struct {
	IPAPath          string
	ExpectedBundleID string
	AdHoc            bool
	JSON             bool
}

// PlanSigning extracts an unsigned IPA into a private temporary directory and
// prints the signing plan. It reads no credential, makes no network request and
// executes nothing from the IPA. Output names bundle identifiers, so run it
// locally, never in a public workflow log, for a private application.
func PlanSigning(options *PlanSigningOptions, out io.Writer) error {
	if options == nil || !filepath.IsAbs(options.IPAPath) {
		return errors.New("an absolute IPA path is required")
	}
	info, err := os.Stat(options.IPAPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxDeployIPABytes {
		return errors.New("IPA is not a bounded regular file")
	}
	scratch, err := os.MkdirTemp("", "builder-plan-")
	if err != nil {
		return errors.New("prepare plan workspace")
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	appPath, err := extractUnsignedIPA(options.IPAPath, filepath.Join(scratch, "unsigned"))
	if err != nil {
		return err
	}
	plan, err := planSigning(appPath)
	if err != nil {
		return err
	}
	if options.ExpectedBundleID != "" && options.ExpectedBundleID != plan.MainBundleID {
		return errors.New("application identifier does not match the expected bundle identifier")
	}
	profileType := ascAppStoreProfileType
	if options.AdHoc {
		profileType = ascAdHocProfileType
	}
	if options.JSON {
		return plan.renderJSON(out, profileType)
	}
	plan.render(out, profileType)
	return nil
}
