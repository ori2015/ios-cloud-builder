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
// they are not embedded in the app, but Xcode still writes them to
// <App>.app.xcent in the intermediates directory. It is best effort: a missing
// file only means the profile's own entitlements are used, as before.
func copyEntitlementsRequest(derivedData, appPath, configuration string, log io.Writer) {
	name := filepath.Base(appPath) + ".xcent"
	root := filepath.Join(derivedData, "Build", "Intermediates.noindex")
	var matches []string
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !entry.IsDir() && entry.Name() == name && strings.Contains(path, string(filepath.Separator)+configuration+"-iphoneos"+string(filepath.Separator)) {
			matches = append(matches, path)
		}
		return nil
	})
	if len(matches) == 0 {
		_, _ = fmt.Fprintln(log, "No build entitlements file was found; the provisioning profile entitlements will be used as-is.")
		return
	}
	sort.Strings(matches)
	data, err := readBoundedRegularFile(matches[0], maxEntitlementsRequestBytes)
	if err != nil {
		_, _ = fmt.Fprintln(log, "The build entitlements file could not be read; the provisioning profile entitlements will be used as-is.")
		return
	}
	if err := os.WriteFile(filepath.Join(appPath, entitlementsRequestFile), data, 0600); err != nil {
		_, _ = fmt.Fprintln(log, "The build entitlements file could not be recorded; the provisioning profile entitlements will be used as-is.")
	}
}

// takeAssociatedDomainsRequest reads and deletes the request file from an
// extracted application and returns the sanitised domains. The file is always
// removed so it can never be signed into the bundle. Problems never fail the
// build; they only mean no domains are requested.
func takeAssociatedDomainsRequest(appPath string, log io.Writer) []string {
	path := filepath.Join(appPath, entitlementsRequestFile)
	data, err := readBoundedRegularFile(path, maxEntitlementsRequestBytes)
	_ = os.Remove(path)
	if err != nil {
		return nil
	}
	var request struct {
		Domains []string `plist:"com.apple.developer.associated-domains"`
	}
	if _, err := plist.Unmarshal(data, &request); err != nil {
		_, _ = fmt.Fprintln(log, "The recorded build entitlements were not a valid property list and were ignored.")
		return nil
	}
	kept, dropped := sanitizeAssociatedDomains(request.Domains)
	_, _ = fmt.Fprintf(log, "Accepted %d associated domain request(s); ignored %d.\n", len(kept), dropped)
	return kept
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
