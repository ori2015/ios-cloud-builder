package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// signingTools are the operations that need macOS (security, codesign). They sit
// behind an interface so the plan execution is testable on Linux.
type signingTools interface {
	// ParseProfile decodes a .mobileprovision file.
	ParseProfile(profilePath string) (provisioningProfile, error)
	// InstallProfile makes the profile known to codesign's private HOME.
	InstallProfile(profilePath, uuid string) error
	// Codesign signs one target with the distribution identity. entitlements is
	// empty for code without a profile.
	Codesign(target, entitlements string) error
	// Verify checks the finished application.
	Verify(appPath string) error
}

// profileProvider is the App Store Connect side of profile discovery.
type profileProvider interface {
	// Candidates downloads the active profiles of exactly this bundle id into dir
	// and returns the bundle's ASC resource id.
	Candidates(ctx context.Context, bundleID, dir string) (resourceID string, paths []string, err error)
	// Create makes a new profile for the bundle and returns its path.
	Create(ctx context.Context, resourceID, bundleID, fingerprint, dir string) (string, error)
}

// planSigner carries everything needed to execute a signingPlan.
type planSigner struct {
	tools       signingTools
	provider    profileProvider // nil when no ASC API credentials are available
	static      []string        // protected fallback profile files (already on disk)
	teamID      string
	fingerprint string
	profileType string
	secretsDir  string
	log         io.Writer
	// associatedDomains apply to the main application only.
	associatedDomains []string
}

// signPlan signs every step inside-out. It selects or creates one profile per
// profile-bearing bundle with the same rules the main application always used,
// embeds it, derives the entitlements from it, and signs. Any failure aborts the
// whole run; nothing is reported as signed unless every step succeeded.
func (s *planSigner) signPlan(ctx context.Context, plan *signingPlan) error {
	staticCandidates := make([]provisioningProfileCandidate, 0, len(s.static))
	for _, candidatePath := range s.static {
		profile, err := s.tools.ParseProfile(candidatePath)
		if err != nil {
			return err
		}
		staticCandidates = append(staticCandidates, provisioningProfileCandidate{path: candidatePath, profile: profile})
	}
	granted := map[string]int{} // bundle id -> number of App Groups, for the consistency note
	index := 0
	for _, step := range plan.Steps {
		step := step
		if !step.NeedsProfile {
			if err := s.tools.Codesign(step.Path, ""); err != nil {
				return err
			}
			continue
		}
		index++
		selected, err := s.profileFor(ctx, &step, index, staticCandidates)
		if err != nil {
			return fmt.Errorf("%s %s: %w", step.Kind, step.RelPath, err)
		}
		profile := selected.profile
		if s.profileType == ascAdHocProfileType {
			_, _ = fmt.Fprintf(s.log, "Ad hoc profile %q provisions %d device(s).\n", profile.Name, len(profile.ProvisionedDevices))
		}
		if err := s.tools.InstallProfile(selected.path, profile.UUID); err != nil {
			return fmt.Errorf("install provisioning profile")
		}
		if err := copyPrivateFile(selected.path, filepath.Join(step.Path, "embedded.mobileprovision")); err != nil {
			return fmt.Errorf("embed provisioning profile")
		}
		entitlements := profile.Entitlements
		domains := []string(nil)
		if step.Kind == kindApp {
			domains = s.associatedDomains
		} else {
			entitlements = distributionEntitlements(entitlements, s.teamID)
		}
		entitlementsPath := filepath.Join(s.secretsDir, fmt.Sprintf("entitlements-%03d.plist", index))
		if err := writeSigningEntitlements(entitlementsPath, entitlements, domains); err != nil {
			return fmt.Errorf("prepare signing entitlements")
		}
		granted[step.BundleID] = appGroupCount(profile.Entitlements)
		if err := s.tools.Codesign(step.Path, entitlementsPath); err != nil {
			return err
		}
	}
	s.noteAppGroups(plan, granted)
	return s.tools.Verify(plan.main().Path)
}

// profileFor mirrors the original single-application flow for one bundle id:
// protected fallback profiles plus the active profiles ASC lists for exactly
// that identifier, then creation of a missing profile bound to the imported
// certificate.
func (s *planSigner) profileFor(ctx context.Context, step *signingTarget, index int, static []provisioningProfileCandidate) (provisioningProfileCandidate, error) {
	dir := filepath.Join(s.secretsDir, fmt.Sprintf("profiles-%03d", index))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return provisioningProfileCandidate{}, errors.New("prepare profile directory")
	}
	candidates := append([]provisioningProfileCandidate(nil), static...)
	var resourceID string
	if s.provider != nil {
		var paths []string
		var err error
		resourceID, paths, err = s.provider.Candidates(ctx, step.BundleID, dir)
		if err != nil {
			if len(static) == 0 {
				return provisioningProfileCandidate{}, err
			}
			_, _ = fmt.Fprintf(s.log, "App Store Connect profile discovery failed for %s: %v\nTrying protected fallback profiles.\n", step.BundleID, err)
		}
		for _, downloaded := range paths {
			profile, parseErr := s.tools.ParseProfile(downloaded)
			if parseErr != nil {
				return provisioningProfileCandidate{}, parseErr
			}
			candidates = append(candidates, provisioningProfileCandidate{path: downloaded, profile: profile})
		}
	}
	selected, selectionErr := selectProvisioningProfile(candidates, s.teamID, step.BundleID, s.fingerprint, s.profileType)
	if selectionErr != nil && s.provider != nil && resourceID != "" {
		createdPath, err := s.provider.Create(ctx, resourceID, step.BundleID, s.fingerprint, dir)
		if err != nil {
			return provisioningProfileCandidate{}, err
		}
		profile, err := s.tools.ParseProfile(createdPath)
		if err != nil {
			return provisioningProfileCandidate{}, err
		}
		candidates = append(candidates, provisioningProfileCandidate{path: createdPath, profile: profile})
		selected, selectionErr = selectProvisioningProfile(candidates, s.teamID, step.BundleID, s.fingerprint, s.profileType)
		if selectionErr == nil {
			_, _ = fmt.Fprintf(s.log, "Created a %s provisioning profile for %s through the App Store Connect API.\n", s.profileType, step.BundleID)
		}
	}
	return selected, selectionErr
}

// distributionEntitlements guarantees the minimum an App Store signature needs,
// on top of whatever the profile grants. It copies; the profile map is untouched.
// The profile stays the only authority for every other entitlement.
func distributionEntitlements(profile map[string]any, teamID string) map[string]any {
	out := make(map[string]any, len(profile)+2)
	for key, value := range profile {
		out[key] = value
	}
	if _, ok := out["get-task-allow"]; !ok {
		out["get-task-allow"] = false
	}
	if _, ok := out["com.apple.developer.team-identifier"]; !ok {
		out["com.apple.developer.team-identifier"] = teamID
	}
	return out
}

func appGroupCount(entitlements map[string]any) int {
	switch groups := entitlements["com.apple.security.application-groups"].(type) {
	case []any:
		return len(groups)
	case []string:
		return len(groups)
	}
	return 0
}

// noteAppGroups logs, by count only, when a bundle that shares data through App
// Groups has a different situation from the main application. A widget or Watch
// extension whose Bundle ID was never added to the same App Group in the Apple
// developer portal signs fine but cannot read the shared container.
func (s *planSigner) noteAppGroups(plan *signingPlan, granted map[string]int) {
	mainGroups := granted[plan.MainBundleID]
	ids := make([]string, 0, len(granted))
	for id := range granted {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if id == plan.MainBundleID {
			continue
		}
		if (granted[id] == 0) != (mainGroups == 0) {
			_, _ = fmt.Fprintf(s.log, "Note: %s has %d App Group(s) and the main application has %d; shared containers need the same group on both Bundle IDs.\n",
				strings.TrimPrefix(id, plan.MainBundleID), granted[id], mainGroups)
		}
	}
}

// setPlanBuildNumbers applies the build number to the main application and to
// every nested app-like bundle: App Store Connect requires an extension's or
// Watch app's CFBundleVersion to follow the containing app.
func setPlanBuildNumbers(plan *signingPlan, buildNumber string) error {
	for index := range plan.Steps {
		if plan.Steps[index].SetsBuildNumber {
			if err := setBundleBuildNumber(plan.Steps[index].InfoPath, buildNumber); err != nil {
				return err
			}
		}
	}
	return nil
}
