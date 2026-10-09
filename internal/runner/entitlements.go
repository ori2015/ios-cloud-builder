package runner

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"howett.net/plist"
)

// Apple provisioning profiles list Associated Domains as the wildcard "*", so
// the profile alone can never yield an app that declares a concrete
// "applinks:<host>" entry, and Universal Links then never associate with the
// app. The project's own entitlements are therefore consulted for exactly this
// one key, and only for a deliberately narrow value shape.
//
// Trust boundary: the request file is produced by untrusted project code. It is
// parsed and sanitised in the isolated trusted-packaging job, carried to the
// signing job inside the authenticated provenance manifest, and re-validated
// there. Every other project-declared entitlement is ignored: the profile stays
// the only authority for them. Claiming a host the project does not control is
// harmless because iOS also requires that host to publish a matching
// apple-app-site-association file.
const (
	entitlementsRequestFile     = ".entitlements-request.xcent"
	associatedDomainsKey        = "com.apple.developer.associated-domains"
	maxEntitlementsRequestBytes = 64 * 1024
	maxAssociatedDomains        = 20
	maxAssociatedDomainLength   = 253 + len("applinks:*.")
)

// applinksDomainPattern accepts only applinks:<hostname> with an optional
// leading "*." label. Query parameters such as ?mode=developer, other service
// prefixes, ports, paths and single-label hosts are rejected.
var applinksDomainPattern = regexp.MustCompile(
	`^applinks:(\*\.)?[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)+$`)

// sanitizeAssociatedDomains keeps the valid, de-duplicated entries in a stable
// order and reports how many were discarded. It never returns the discarded
// values so callers can log the count without echoing project-controlled text.
func sanitizeAssociatedDomains(values []string) (kept []string, dropped int) {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if len(value) > maxAssociatedDomainLength || !applinksDomainPattern.MatchString(value) || seen[value] {
			dropped++
			continue
		}
		seen[value] = true
		kept = append(kept, value)
	}
	sort.Strings(kept)
	if len(kept) > maxAssociatedDomains {
		dropped += len(kept) - maxAssociatedDomains
		kept = kept[:maxAssociatedDomains]
	}
	return kept, dropped
}

// validateAssociatedDomains is the strict, fail-closed form used on data read
// back from the manifest: anything sanitising would have changed is an error.
func validateAssociatedDomains(values []string) error {
	kept, dropped := sanitizeAssociatedDomains(values)
	if dropped != 0 || len(kept) != len(values) {
		return errors.New("invalid associated domains in provenance manifest")
	}
	for i := range kept {
		if kept[i] != values[i] {
			return errors.New("invalid associated domains in provenance manifest")
		}
	}
	return nil
}

// copyEntitlementsRequest makes the entitlements Xcode computed for the
// unsigned build available to the trusted packager. With code signing disabled
// they are not embedded in the app, but Xcode still writes them to a
// "<name>.app.xcent" file in the intermediates directory. The name follows the
// target, which need not match the product name, so every .xcent of the
// configuration is considered and the first that requests Associated Domains is
// used. It reports whether a request was recorded; a miss only means the
// profile's own entitlements are used, as before.
func copyEntitlementsRequest(derivedData, appPath, configuration string, log io.Writer) bool {
	root := filepath.Join(derivedData, "Build", "Intermediates.noindex")
	marker := string(filepath.Separator) + configuration + "-iphoneos" + string(filepath.Separator)
	var matches []string
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".app.xcent") && strings.Contains(path, marker) {
			matches = append(matches, path)
		}
		return nil
	})
	sort.Strings(matches)
	for _, match := range matches {
		data, err := readBoundedRegularFile(match, maxEntitlementsRequestBytes)
		if err != nil || !requestsAssociatedDomains(data) {
			continue
		}
		if err := os.WriteFile(filepath.Join(appPath, entitlementsRequestFile), data, 0600); err != nil {
			_, _ = fmt.Fprintln(log, "The build entitlements file could not be recorded.")
			return false
		}
		return true
	}
	_, _ = fmt.Fprintf(log, "Examined %d build entitlements file(s); none requested associated domains.\n", len(matches))
	return false
}

// requestsAssociatedDomains reports whether the project entitlements ask for
// anything this package honours: associated domains or iCloud services.
func requestsAssociatedDomains(data []byte) bool {
	var request entitlementsRequest
	_, err := plist.Unmarshal(data, &request)
	return err == nil && (len(request.Domains) > 0 || len(request.ICloudServices) > 0)
}

// entitlementsRequest is the part of the project's entitlements that is read.
type entitlementsRequest struct {
	Domains        []string `plist:"com.apple.developer.associated-domains"`
	ICloudServices []string `plist:"com.apple.developer.icloud-services"`
}

// entitlementsFileFromBuildSettings finds the project's own entitlements file
// from `xcodebuild -showBuildSettings` output. It is the fallback for builds in
// which no .xcent is produced. The file must resolve inside sourceRoot.
func entitlementsFileFromBuildSettings(settings []byte, sourceRoot string) (string, bool) {
	var entitlements, srcroot string
	flush := func() (string, bool) {
		if entitlements == "" || srcroot == "" {
			return "", false
		}
		path := entitlements
		if !filepath.IsAbs(path) {
			path = filepath.Join(srcroot, path)
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || !pathWithin(sourceRoot, resolved) {
			return "", false
		}
		return resolved, true
	}
	for _, line := range strings.Split(string(settings), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "Build settings for action"):
			if path, ok := flush(); ok {
				return path, true
			}
			entitlements, srcroot = "", ""
		case strings.HasPrefix(trimmed, "CODE_SIGN_ENTITLEMENTS = "):
			entitlements = strings.TrimSpace(strings.TrimPrefix(trimmed, "CODE_SIGN_ENTITLEMENTS = "))
		case strings.HasPrefix(trimmed, "SRCROOT = "):
			srcroot = strings.TrimSpace(strings.TrimPrefix(trimmed, "SRCROOT = "))
		}
	}
	return flush()
}

// recordEntitlementsFromSettings is the fallback for copyEntitlementsRequest.
func recordEntitlementsFromSettings(run executor, iosRoot, sourceRoot string, container []string, scheme, configuration, appPath string) {
	args := append(append([]string{}, container...), "-scheme", scheme, "-configuration", configuration,
		"-destination", "generic/platform=iOS", "-showBuildSettings")
	output, err := run.capture(iosRoot, "xcodebuild", args...)
	if err != nil {
		return
	}
	path, ok := entitlementsFileFromBuildSettings(output, sourceRoot)
	if !ok {
		_, _ = fmt.Fprintln(run.log, "No project entitlements file was found in the build settings.")
		return
	}
	data, err := readBoundedRegularFile(path, maxEntitlementsRequestBytes)
	if err != nil || !requestsAssociatedDomains(data) {
		_, _ = fmt.Fprintln(run.log, "The project entitlements file does not request associated domains.")
		return
	}
	if err := os.WriteFile(filepath.Join(appPath, entitlementsRequestFile), data, 0600); err != nil {
		_, _ = fmt.Fprintln(run.log, "The project entitlements file could not be recorded.")
	}
}

// takeAssociatedDomainsRequest reads and deletes the request file from an
// extracted application and returns the sanitised domains. The file is always
// removed so it can never be signed into the bundle. Problems never fail the
// build; they only mean no domains are requested.
func takeAssociatedDomainsRequest(appPath string, log io.Writer) []string {
	return takeEntitlementsRequest(appPath, log).Domains
}

// sanitisedRequest holds the validated values taken from the request file.
type sanitisedRequest struct {
	Domains        []string
	ICloudServices []string
}

// takeEntitlementsRequest is takeAssociatedDomainsRequest plus the requested
// iCloud services, which are kept only if they are on the allowlist.
func takeEntitlementsRequest(appPath string, log io.Writer) sanitisedRequest {
	path := filepath.Join(appPath, entitlementsRequestFile)
	data, err := readBoundedRegularFile(path, maxEntitlementsRequestBytes)
	_ = os.Remove(path)
	if err != nil {
		return sanitisedRequest{}
	}
	var request entitlementsRequest
	if _, err := plist.Unmarshal(data, &request); err != nil {
		_, _ = fmt.Fprintln(log, "The recorded build entitlements were not a valid property list and were ignored.")
		return sanitisedRequest{}
	}
	kept, dropped := sanitizeAssociatedDomains(request.Domains)
	_, _ = fmt.Fprintf(log, "Accepted %d associated domain request(s); ignored %d.\n", len(kept), dropped)
	services := sanitizeICloudServices(request.ICloudServices)
	if len(services) > 0 {
		_, _ = fmt.Fprintf(log, "Accepted %d iCloud service request(s).\n", len(services))
	}
	return sanitisedRequest{Domains: kept, ICloudServices: services}
}

func readBoundedRegularFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("not a bounded regular file")
	}
	return os.ReadFile(path)
}

// mergeAssociatedDomains returns the entitlements to sign with: the profile's
// entitlements, with only the Associated Domains value narrowed to the
// requested hosts the profile permits. The profile's map is never modified.
// A profile without the key, or without any permitted request, is returned
// unchanged, so this can only ever narrow what the profile already grants.
func mergeAssociatedDomains(profileEntitlements map[string]any, requested []string) map[string]any {
	if len(requested) == 0 {
		return profileEntitlements
	}
	current, ok := profileEntitlements[associatedDomainsKey]
	if !ok {
		return profileEntitlements
	}
	allowAll := false
	allowed := map[string]bool{}
	switch value := current.(type) {
	case string:
		allowAll = value == "*"
		allowed[value] = true
	case []any:
		for _, item := range value {
			if text, isText := item.(string); isText {
				allowAll = allowAll || text == "*"
				allowed[text] = true
			}
		}
	case []string:
		for _, text := range value {
			allowAll = allowAll || text == "*"
			allowed[text] = true
		}
	default:
		return profileEntitlements
	}
	granted := make([]string, 0, len(requested))
	for _, domain := range requested {
		if allowAll || allowed[domain] {
			granted = append(granted, domain)
		}
	}
	if len(granted) == 0 {
		return profileEntitlements
	}
	merged := make(map[string]any, len(profileEntitlements))
	for key, value := range profileEntitlements {
		merged[key] = value
	}
	merged[associatedDomainsKey] = granted
	return merged
}

// writeSigningEntitlements writes the property list handed to codesign.
func writeSigningEntitlements(path string, profileEntitlements map[string]any, requestedDomains []string) error {
	data, err := plist.Marshal(mergeAssociatedDomains(profileEntitlements, requestedDomains), plist.XMLFormat)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

const (
	iCloudServicesKey        = "com.apple.developer.icloud-services"
	iCloudEnvironmentKey     = "com.apple.developer.icloud-container-environment"
	iCloudDevContainersKey   = "com.apple.developer.icloud-container-development-container-identifiers"
	iCloudKVStoreKey         = "com.apple.developer.ubiquity-kvstore-identifier"
	maxICloudServiceRequests = 8
)

// allowedICloudServices are the only values a project may request.
var allowedICloudServices = map[string]bool{"CloudKit": true, "CloudDocuments": true, "CloudKit-Anonymous": true}

// sanitizeICloudServices keeps allowlisted, de-duplicated services in a stable order.
func sanitizeICloudServices(values []string) []string {
	seen := map[string]bool{}
	var kept []string
	for _, value := range values {
		if allowedICloudServices[value] && !seen[value] && len(kept) < maxICloudServiceRequests {
			seen[value] = true
			kept = append(kept, value)
		}
	}
	sort.Strings(kept)
	return kept
}

// validateICloudServices rejects anything the sanitiser would not have produced.
func validateICloudServices(values []string) error {
	if len(values) > maxICloudServiceRequests {
		return errors.New("too many iCloud services")
	}
	for _, value := range values {
		if !allowedICloudServices[value] {
			return errors.New("invalid iCloud service")
		}
	}
	return nil
}

// narrowICloudEntitlements turns the profile's iCloud template values into the
// concrete ones an App Store signature needs, the way Xcode does when it
// signs for distribution. Apple profiles carry "*" services, both container
// environments, a development container list and a "TEAM.*" key-value store;
// App Store Connect rejects those in a signature. Every change only narrows
// what the profile already grants. Without iCloud in the profile the map is
// returned unchanged. The profile's map is never modified.
func narrowICloudEntitlements(profile map[string]any, requestedServices []string, teamID, bundleID string) map[string]any {
	_, hasServices := profile[iCloudServicesKey]
	_, hasEnvironment := profile[iCloudEnvironmentKey]
	_, hasKVStore := profile[iCloudKVStoreKey]
	if !hasServices && !hasEnvironment && !hasKVStore {
		return profile
	}
	out := make(map[string]any, len(profile))
	for key, value := range profile {
		out[key] = value
	}
	delete(out, iCloudDevContainersKey)
	if values := stringList(out[iCloudEnvironmentKey]); len(values) > 0 {
		for _, value := range values {
			if value == "Production" {
				out[iCloudEnvironmentKey] = "Production"
			}
		}
	}
	if granted, ok := out[iCloudServicesKey]; ok {
		list := stringList(granted)
		allowAll := false
		allowed := map[string]bool{}
		for _, value := range list {
			allowAll = allowAll || value == "*"
			allowed[value] = true
		}
		var narrowed []string
		for _, service := range requestedServices {
			if allowAll || allowed[service] {
				narrowed = append(narrowed, service)
			}
		}
		switch {
		case len(narrowed) > 0:
			out[iCloudServicesKey] = narrowed
		case allowAll:
			delete(out, iCloudServicesKey)
		}
	}
	if kv, ok := out[iCloudKVStoreKey].(string); ok && teamID != "" && bundleID != "" && strings.HasSuffix(kv, ".*") {
		out[iCloudKVStoreKey] = teamID + "." + bundleID
	}
	return out
}

// stringList reads a plist string or string array.
func stringList(value any) []string {
	switch v := value.(type) {
	case string:
		return []string{v}
	case []string:
		return v
	case []any:
		var out []string
		for _, item := range v {
			if text, ok := item.(string); ok {
				out = append(out, text)
			}
		}
		return out
	}
	return nil
}
