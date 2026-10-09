package runner

import (
	"reflect"
	"testing"
)

func iCloudTemplateProfile() map[string]any {
	return map[string]any{
		"application-identifier": "TEAM1234.com.example.app",
		iCloudServicesKey:        "*",
		iCloudEnvironmentKey:     []any{"Production", "Development"},
		iCloudDevContainersKey:   []any{"iCloud.com.example.app"},
		iCloudKVStoreKey:         "TEAM1234.*",
		"com.apple.developer.icloud-container-identifiers": []any{"iCloud.com.example.app"},
	}
}

func TestNarrowICloudEntitlementsMatchesDistributionSigning(t *testing.T) {
	profile := iCloudTemplateProfile()
	got := narrowICloudEntitlements(profile, []string{"CloudKit"}, "TEAM1234", "com.example.app")
	if got[iCloudEnvironmentKey] != "Production" {
		t.Fatalf("environment = %v", got[iCloudEnvironmentKey])
	}
	if _, present := got[iCloudDevContainersKey]; present {
		t.Fatal("development container identifiers must be removed")
	}
	if !reflect.DeepEqual(got[iCloudServicesKey], []string{"CloudKit"}) {
		t.Fatalf("services = %v", got[iCloudServicesKey])
	}
	if got[iCloudKVStoreKey] != "TEAM1234.com.example.app" {
		t.Fatalf("kvstore = %v", got[iCloudKVStoreKey])
	}
	if profile[iCloudServicesKey] != "*" || len(profile) != 6 {
		t.Fatal("the profile map must not be modified")
	}
}

func TestNarrowICloudEntitlementsNeverWidens(t *testing.T) {
	profile := iCloudTemplateProfile()
	profile[iCloudServicesKey] = []any{"CloudKit"}
	got := narrowICloudEntitlements(profile, []string{"CloudDocuments", "CloudKit"}, "TEAM1234", "com.example.app")
	if !reflect.DeepEqual(got[iCloudServicesKey], []string{"CloudKit"}) {
		t.Fatalf("a service the profile does not grant was added: %v", got[iCloudServicesKey])
	}
}

func TestNarrowICloudEntitlementsWithoutRequestDropsWildcard(t *testing.T) {
	got := narrowICloudEntitlements(iCloudTemplateProfile(), nil, "TEAM1234", "com.example.app")
	if _, present := got[iCloudServicesKey]; present {
		t.Fatal("a wildcard services value is never valid in a signature")
	}
}

func TestNarrowICloudEntitlementsLeavesOtherProfilesAlone(t *testing.T) {
	profile := map[string]any{"application-identifier": "TEAM1234.com.example.app"}
	got := narrowICloudEntitlements(profile, []string{"CloudKit"}, "TEAM1234", "com.example.app")
	if !reflect.DeepEqual(got, profile) {
		t.Fatalf("unexpected change: %v", got)
	}
}

func TestICloudServiceAllowlist(t *testing.T) {
	got := sanitizeICloudServices([]string{"CloudKit", "Bogus", "CloudKit", "CloudDocuments"})
	if !reflect.DeepEqual(got, []string{"CloudDocuments", "CloudKit"}) {
		t.Fatalf("sanitized = %v", got)
	}
	if validateICloudServices([]string{"Bogus"}) == nil || validateICloudServices(got) != nil {
		t.Fatal("validation must reject only values outside the allowlist")
	}
}
