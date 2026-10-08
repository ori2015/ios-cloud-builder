package runner

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha1" // #nosec G505 -- test fixture mirrors Apple's SHA-1 certificate fingerprints.
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"howett.net/plist"
)

const testMainID = "com.example.app"

type fixture struct {
	t   *testing.T
	app string
}

func newFixture(t *testing.T) *fixture {
	app := filepath.Join(t.TempDir(), "App.app")
	f := &fixture{t: t, app: app}
	f.bundle(".", map[string]any{"CFBundleIdentifier": testMainID, "CFBundleShortVersionString": "1.0", "CFBundleExecutable": "App"})
	return f
}

// bundle writes <rel>/Info.plist (and a fake executable) below the app.
func (f *fixture) bundle(rel string, info map[string]any) {
	f.t.Helper()
	dir := filepath.Join(f.app, rel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	data, err := plist.Marshal(info, plist.XMLFormat)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Info.plist"), data, 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) file(rel string) {
	f.t.Helper()
	path := filepath.Join(f.app, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) appex(rel, id, point string) {
	f.bundle(rel, map[string]any{"CFBundleIdentifier": id, "CFBundleShortVersionString": "1.0",
		"NSExtension": map[string]any{"NSExtensionPointIdentifier": point}})
}

func (f *fixture) plan() (*signingPlan, error) { return planSigning(f.app) }

func stepNames(plan *signingPlan) []string {
	var out []string
	for _, step := range plan.Steps {
		out = append(out, string(step.Kind)+":"+step.RelPath)
	}
	return out
}

func TestPlanSingleAppHasOneStep(t *testing.T) {
	plan, err := newFixture(t).plan()
	if err != nil || len(plan.Steps) != 1 || plan.main().Kind != kindApp || !plan.main().NeedsProfile {
		t.Fatalf("plan = %+v, err = %v", plan, err)
	}
}

func TestPlanFlowSipLayoutSignsInsideOut(t *testing.T) {
	f := newFixture(t)
	f.appex("PlugIns/Widget.appex", testMainID+".widget", "com.apple.widgetkit-extension")
	f.bundle("Frameworks/Lib.framework", map[string]any{"CFBundleIdentifier": "org.vendor.lib"})
	f.file("Frameworks/Lib.framework/Lib")
	f.file("Frameworks/libswiftCore.dylib")
	f.bundle("PlugIns/Widget.appex/Frameworks/Inner.framework", map[string]any{"CFBundleIdentifier": "org.vendor.inner"})
	plan, err := f.plan()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"framework:Frameworks/Lib.framework", "dylib:Frameworks/libswiftCore.dylib",
		"framework:PlugIns/Widget.appex/Frameworks/Inner.framework", "app-extension:PlugIns/Widget.appex", "app:.",
	}
	if got := stepNames(plan); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v\nwant    %v", got, want)
	}
	if len(plan.profileTargets()) != 2 {
		t.Fatalf("profile targets = %d", len(plan.profileTargets()))
	}
}

func TestPlanWatchAppClipAndNestedExtensions(t *testing.T) {
	f := newFixture(t)
	f.bundle("Watch/Watch.app", map[string]any{"CFBundleIdentifier": testMainID + ".watchkitapp", "CFBundleShortVersionString": "1.0",
		"WKWatchKitApp": true, "WKCompanionAppBundleIdentifier": testMainID, "DTPlatformName": "watchos"})
	f.appex("Watch/Watch.app/PlugIns/Ext.appex", testMainID+".watchkitapp.extension", "com.apple.watchkit")
	f.bundle("AppClips/Clip.app", map[string]any{"CFBundleIdentifier": testMainID + ".Clip", "CFBundleShortVersionString": "1.0",
		"NSAppClip": map[string]any{}})
	f.appex("PlugIns/Share.appex", testMainID+".share", "com.apple.share-services")
	plan, err := f.plan()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"app-clip:AppClips/Clip.app", "app-extension:PlugIns/Share.appex",
		"app-extension:Watch/Watch.app/PlugIns/Ext.appex", "watch-app:Watch/Watch.app", "app:.",
	}
	if got := stepNames(plan); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v\nwant    %v", got, want)
	}
	for _, step := range plan.Steps {
		wantPlatform := platformIOS
		if strings.HasPrefix(step.RelPath, "Watch/") {
			wantPlatform = platformWatchOS
		}
		if step.Platform != wantPlatform {
			t.Errorf("%s platform = %s", step.RelPath, step.Platform)
		}
	}
	if len(plan.Warnings) != 0 {
		t.Fatalf("warnings = %v", plan.Warnings)
	}
}

func TestPlanXPCServiceGetsNoProfile(t *testing.T) {
	f := newFixture(t)
	f.bundle("XPCServices/Helper.xpc", map[string]any{"CFBundleIdentifier": testMainID + ".helper", "CFBundleShortVersionString": "1.0"})
	plan, err := f.plan()
	if err != nil {
		t.Fatal(err)
	}
	if got := stepNames(plan); got[0] != "xpc-service:XPCServices/Helper.xpc" || plan.Steps[0].NeedsProfile || plan.Steps[0].SetsBuildNumber {
		t.Fatalf("plan = %v", got)
	}
}

func TestPlanRejectsUnsafeOrUnknownLayouts(t *testing.T) {
	good := func(f *fixture, rel, id string) { f.appex(rel, id, "com.apple.widgetkit-extension") }
	cases := map[string]func(f *fixture){
		"prefix is another app": func(f *fixture) { good(f, "PlugIns/W.appex", "com.other.app.widget") },
		"prefix without dot":    func(f *fixture) { good(f, "PlugIns/W.appex", testMainID+"widget") },
		"same as main":          func(f *fixture) { good(f, "PlugIns/W.appex", testMainID) },
		"empty label":           func(f *fixture) { good(f, "PlugIns/W.appex", testMainID+"..x") },
		"trailing dot":          func(f *fixture) { good(f, "PlugIns/W.appex", testMainID+".") },
		"underscore":            func(f *fixture) { good(f, "PlugIns/W.appex", testMainID+".a_b") },
		"duplicate ids": func(f *fixture) {
			good(f, "PlugIns/A.appex", testMainID+".w")
			good(f, "PlugIns/B.appex", testMainID+".w")
		},
		"appex without extension point": func(f *fixture) {
			f.bundle("PlugIns/W.appex", map[string]any{"CFBundleIdentifier": testMainID + ".w"})
		},
		"appex outside PlugIns": func(f *fixture) { good(f, "Other/W.appex", testMainID+".w") },
		"app outside containers": func(f *fixture) {
			f.bundle("Stuff/Nested.App", map[string]any{"CFBundleIdentifier": testMainID + ".n"})
		},
		"xpc outside XPCServices": func(f *fixture) { f.bundle("Stuff/S.xpc", map[string]any{"CFBundleIdentifier": testMainID + ".s"}) },
		"case variant container":  func(f *fixture) { good(f, "plugins/W.appex", testMainID+".w") },
		"nested container below root": func(f *fixture) {
			good(f, "Resources/PlugIns/W.appex", testMainID+".w")
		},
		"app in PlugIns": func(f *fixture) {
			f.bundle("PlugIns/W.app", map[string]any{"CFBundleIdentifier": testMainID + ".w"})
		},
		"stray file in container": func(f *fixture) {
			good(f, "PlugIns/W.appex", testMainID+".w")
			f.file("PlugIns/readme.txt")
		},
		"empty container": func(f *fixture) {
			if err := os.MkdirAll(filepath.Join(f.app, "Watch"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"missing nested Info.plist": func(f *fixture) {
			if err := os.MkdirAll(filepath.Join(f.app, "PlugIns", "W.appex"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"watch inside appex": func(f *fixture) {
			good(f, "PlugIns/W.appex", testMainID+".w")
			f.bundle("PlugIns/W.appex/Watch/X.app", map[string]any{"CFBundleIdentifier": testMainID + ".x"})
		},
		"app clip inside watch app": func(f *fixture) {
			f.bundle("Watch/W.app", map[string]any{"CFBundleIdentifier": testMainID + ".w", "WKWatchKitApp": true})
			f.bundle("Watch/W.app/AppClips/C.app", map[string]any{"CFBundleIdentifier": testMainID + ".w.c"})
		},
		"bundle inside framework": func(f *fixture) {
			f.bundle("Frameworks/L.framework", map[string]any{"CFBundleIdentifier": "org.v.l"})
			f.appex("Frameworks/L.framework/PlugIns/W.appex", testMainID+".w", "x")
		},
		"watch companion names other app": func(f *fixture) {
			f.bundle("Watch/W.app", map[string]any{"CFBundleIdentifier": testMainID + ".w", "WKCompanionAppBundleIdentifier": "com.other.app"})
		},
		"simulator build": func(f *fixture) {
			f.bundle("PlugIns/W.appex", map[string]any{"CFBundleIdentifier": testMainID + ".w", "DTPlatformName": "iphonesimulator",
				"NSExtension": map[string]any{"NSExtensionPointIdentifier": "x"}})
		},
		"watch platform on ios extension": func(f *fixture) {
			f.bundle("PlugIns/W.appex", map[string]any{"CFBundleIdentifier": testMainID + ".w", "DTPlatformName": "watchos",
				"NSExtension": map[string]any{"NSExtensionPointIdentifier": "x"}})
		},
		"too deep": func(f *fixture) {
			f.bundle("Watch/W.app", map[string]any{"CFBundleIdentifier": testMainID + ".w", "WKWatchKitApp": true})
			f.appex("Watch/W.app/PlugIns/E.appex", testMainID+".w.e", "x")
			f.bundle("Watch/W.app/PlugIns/E.appex/Frameworks/A.framework", map[string]any{})
			f.bundle("Watch/W.app/PlugIns/E.appex/Frameworks/A.framework/Frameworks/B.framework", map[string]any{})
			f.bundle("Watch/W.app/PlugIns/E.appex/Frameworks/A.framework/Frameworks/B.framework/Frameworks/C.framework", map[string]any{})
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			mutate(f)
			if plan, err := f.plan(); err == nil {
				t.Fatalf("accepted: %v", stepNames(plan))
			}
		})
	}
}

func TestPlanRejectsSymlinksAndSpecialFiles(t *testing.T) {
	for name, make := range map[string]func(f *fixture) error{
		"symlink file": func(f *fixture) error { return os.Symlink("/etc/passwd", filepath.Join(f.app, "link")) },
		"symlink dir":  func(f *fixture) error { return os.Symlink("/tmp", filepath.Join(f.app, "Frameworks")) },
		"symlink appex": func(f *fixture) error {
			if err := os.MkdirAll(filepath.Join(f.app, "PlugIns"), 0o755); err != nil {
				return err
			}
			return os.Symlink("../../outside.appex", filepath.Join(f.app, "PlugIns", "W.appex"))
		},
		"symlinked dylib": func(f *fixture) error { return os.Symlink("/usr/lib/libSystem.dylib", filepath.Join(f.app, "x.dylib")) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			if err := make(f); err != nil {
				t.Fatal(err)
			}
			if _, err := f.plan(); err == nil {
				t.Fatal("symlink accepted")
			}
		})
	}
	t.Run("app itself is a symlink", func(t *testing.T) {
		f := newFixture(t)
		link := filepath.Join(t.TempDir(), "Link.app")
		if err := os.Symlink(f.app, link); err != nil {
			t.Fatal(err)
		}
		if _, err := planSigning(link); err == nil {
			t.Fatal("symlinked app accepted")
		}
	})
}

func TestPlanBoundsBundleCount(t *testing.T) {
	f := newFixture(t)
	for i := 0; i <= maxPlanBundles; i++ {
		f.appex(fmt.Sprintf("PlugIns/E%03d.appex", i), fmt.Sprintf("%s.e%d", testMainID, i), "x")
	}
	if _, err := f.plan(); err == nil {
		t.Fatal("too many bundles accepted")
	}
}

func TestPlanWarnsAboutIncompleteMetadata(t *testing.T) {
	f := newFixture(t)
	f.bundle("Watch/W.app", map[string]any{"CFBundleIdentifier": testMainID + ".w", "CFBundleShortVersionString": "1.0"})
	plan, err := f.plan()
	if err != nil || len(plan.Warnings) != 2 {
		t.Fatalf("warnings = %v, err = %v", plan.Warnings, err)
	}
}

func TestPlanRendersWithoutCredentials(t *testing.T) {
	f := newFixture(t)
	f.appex("PlugIns/Widget.appex", testMainID+".widget", "com.apple.widgetkit-extension")
	plan, err := f.plan()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	plan.render(&out, ascAppStoreProfileType)
	for _, want := range []string{"Provisioning profiles required: 2", "app-extension", testMainID + ".widget", "IOS_APP_STORE for exactly " + testMainID + ".widget"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in\n%s", want, out.String())
		}
	}
	out.Reset()
	if err := plan.renderJSON(&out, ascAdHocProfileType); err != nil || !strings.Contains(out.String(), `"profile_type": "IOS_APP_ADHOC"`) {
		t.Fatalf("json = %s, err = %v", out.String(), err)
	}
}

func TestSetPlanBuildNumbersTouchesAppsAndExtensionsOnly(t *testing.T) {
	f := newFixture(t)
	f.appex("PlugIns/W.appex", testMainID+".w", "x")
	f.bundle("Frameworks/L.framework", map[string]any{"CFBundleIdentifier": "org.v.l", "CFBundleVersion": "7"})
	f.bundle("XPCServices/S.xpc", map[string]any{"CFBundleIdentifier": testMainID + ".s", "CFBundleVersion": "7"})
	plan, err := f.plan()
	if err != nil {
		t.Fatal(err)
	}
	if err := setPlanBuildNumbers(plan, "42"); err != nil {
		t.Fatal(err)
	}
	version := func(rel string) string {
		data, _ := os.ReadFile(filepath.Join(f.app, rel, "Info.plist"))
		var v struct {
			V string `plist:"CFBundleVersion"`
		}
		_, _ = plist.Unmarshal(data, &v)
		return v.V
	}
	if version(".") != "42" || version("PlugIns/W.appex") != "42" || version("Frameworks/L.framework") != "7" || version("XPCServices/S.xpc") != "7" {
		t.Fatal("build numbers applied to the wrong bundles")
	}
}

// ---------------------------------------------------------------------------
// Plan execution with fake macOS tools and a fake ASC provider.

var testCert = []byte("distribution certificate bytes")

func testFingerprint() string {
	sum := sha1.Sum(testCert) // #nosec G401 -- fixture.
	return hex.EncodeToString(sum[:])
}

func testProfile(bundleID string, groups ...string) provisioningProfile {
	entitlements := map[string]any{
		"application-identifier":                 "TEAM123456." + bundleID,
		"com.apple.developer.team-identifier":    "TEAM123456",
		"get-task-allow":                         false,
		"com.apple.developer.associated-domains": "*",
	}
	if len(groups) > 0 {
		list := make([]any, len(groups))
		for i, g := range groups {
			list[i] = g
		}
		entitlements["com.apple.security.application-groups"] = list
	}
	return provisioningProfile{
		UUID: "00000000-0000-0000-0000-" + fmt.Sprintf("%012d", len(bundleID)), Name: "p " + bundleID,
		ExpirationDate: time.Now().Add(48 * time.Hour), Platform: []string{"iOS"}, TeamIdentifier: []string{"TEAM123456"},
		DeveloperCertificates: [][]byte{testCert}, Entitlements: entitlements,
	}
}

type fakeTools struct {
	profiles map[string]provisioningProfile // by path
	signed   []string
	ents     map[string]string // target -> entitlements path
	installs []string
	verified string
	failOn   string
}

func (f *fakeTools) ParseProfile(path string) (provisioningProfile, error) {
	p, ok := f.profiles[path]
	if !ok {
		return provisioningProfile{}, errors.New("unknown profile " + path)
	}
	return p, nil
}
func (f *fakeTools) InstallProfile(path, uuid string) error {
	f.installs = append(f.installs, uuid)
	return nil
}
func (f *fakeTools) Codesign(target, entitlements string) error {
	if f.failOn != "" && strings.HasSuffix(target, f.failOn) {
		return errors.New("codesign failed")
	}
	f.signed = append(f.signed, target)
	if f.ents == nil {
		f.ents = map[string]string{}
	}
	f.ents[target] = entitlements
	return nil
}
func (f *fakeTools) Verify(app string) error { f.verified = app; return nil }

type fakeProvider struct {
	tools    *fakeTools
	existing map[string][]provisioningProfile // by bundle id
	created  []string
	missing  map[string]bool // bundle ids unknown to ASC
	queried  []string
}

func (p *fakeProvider) Candidates(_ context.Context, bundleID, dir string) (string, []string, error) {
	p.queried = append(p.queried, bundleID)
	if p.missing[bundleID] {
		return "", nil, errors.New("no exact iOS-compatible bundle identifier exists in App Store Connect")
	}
	var paths []string
	for i, profile := range p.existing[bundleID] {
		path := filepath.Join(dir, fmt.Sprintf("asc-%d.mobileprovision", i))
		if err := os.WriteFile(path, []byte("profile"), 0o600); err != nil {
			return "", nil, err
		}
		p.tools.profiles[path] = profile
		paths = append(paths, path)
	}
	return "res-" + bundleID, paths, nil
}

func (p *fakeProvider) Create(_ context.Context, resourceID, bundleID, fingerprint, dir string) (string, error) {
	if resourceID != "res-"+bundleID || fingerprint != testFingerprint() {
		return "", errors.New("create bound to the wrong bundle or certificate")
	}
	p.created = append(p.created, bundleID)
	path := filepath.Join(dir, "created.mobileprovision")
	if err := os.WriteFile(path, []byte("profile"), 0o600); err != nil {
		return "", err
	}
	p.tools.profiles[path] = testProfile(bundleID)
	return path, nil
}

func newSigner(t *testing.T) (*planSigner, *fakeTools, *fakeProvider, *bytes.Buffer) {
	tools := &fakeTools{profiles: map[string]provisioningProfile{}}
	provider := &fakeProvider{tools: tools, existing: map[string][]provisioningProfile{}, missing: map[string]bool{}}
	var log bytes.Buffer
	return &planSigner{
		tools: tools, provider: provider, teamID: "TEAM123456", fingerprint: testFingerprint(),
		profileType: ascAppStoreProfileType, secretsDir: t.TempDir(), log: &log,
	}, tools, provider, &log
}

func widgetPlan(t *testing.T) (*fixture, *signingPlan) {
	f := newFixture(t)
	f.appex("PlugIns/Widget.appex", testMainID+".widget", "com.apple.widgetkit-extension")
	f.bundle("Frameworks/Lib.framework", map[string]any{"CFBundleIdentifier": "org.vendor.lib"})
	plan, err := f.plan()
	if err != nil {
		t.Fatal(err)
	}
	return f, plan
}

func TestSignPlanSignsInsideOutWithPerBundleProfiles(t *testing.T) {
	f, plan := widgetPlan(t)
	signer, tools, provider, _ := newSigner(t)
	provider.existing[testMainID] = []provisioningProfile{testProfile(testMainID, "group.com.example")}
	provider.existing[testMainID+".widget"] = []provisioningProfile{testProfile(testMainID+".widget", "group.com.example")}
	signer.associatedDomains = []string{"applinks:example.com"}
	if err := signer.signPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, target := range tools.signed {
		order = append(order, rel(f.app, target))
	}
	if strings.Join(order, ",") != "Frameworks/Lib.framework,PlugIns/Widget.appex,." {
		t.Fatalf("signing order = %v", order)
	}
	if tools.verified != f.app {
		t.Fatalf("verified %q", tools.verified)
	}
	// Frameworks: identity only, no profile, no entitlements.
	if tools.ents[filepath.Join(f.app, "Frameworks/Lib.framework")] != "" {
		t.Fatal("framework signed with entitlements")
	}
	if _, err := os.Stat(filepath.Join(f.app, "Frameworks/Lib.framework/embedded.mobileprovision")); err == nil {
		t.Fatal("framework received a provisioning profile")
	}
	for _, rel := range []string{"embedded.mobileprovision", "PlugIns/Widget.appex/embedded.mobileprovision"} {
		if _, err := os.Stat(filepath.Join(f.app, rel)); err != nil {
			t.Errorf("%s not embedded", rel)
		}
	}
	if fmt.Sprint(provider.queried) != fmt.Sprint([]string{testMainID + ".widget", testMainID}) {
		t.Fatalf("ASC queried for %v", provider.queried)
	}
	if len(tools.installs) != 2 {
		t.Fatalf("installs = %v", tools.installs)
	}
}

func readEntitlements(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if _, err := plist.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSignPlanDerivesEntitlementsFromEachProfile(t *testing.T) {
	f, plan := widgetPlan(t)
	signer, tools, provider, _ := newSigner(t)
	mainProfile := testProfile(testMainID, "group.main")
	widgetProfile := testProfile(testMainID+".widget", "group.widget")
	delete(widgetProfile.Entitlements, "get-task-allow") // minimal profile still gets get-task-allow=false
	delete(widgetProfile.Entitlements, "com.apple.developer.team-identifier")
	provider.existing[testMainID] = []provisioningProfile{mainProfile}
	provider.existing[testMainID+".widget"] = []provisioningProfile{widgetProfile}
	signer.associatedDomains = []string{"applinks:example.com"}
	if err := signer.signPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	app := readEntitlements(t, tools.ents[f.app])
	if app["application-identifier"] != "TEAM123456."+testMainID || fmt.Sprint(app["com.apple.developer.associated-domains"]) != "[applinks:example.com]" ||
		fmt.Sprint(app["com.apple.security.application-groups"]) != "[group.main]" {
		t.Fatalf("app entitlements = %v", app)
	}
	widget := readEntitlements(t, tools.ents[filepath.Join(f.app, "PlugIns/Widget.appex")])
	if widget["application-identifier"] != "TEAM123456."+testMainID+".widget" || widget["get-task-allow"] != false ||
		widget["com.apple.developer.team-identifier"] != "TEAM123456" || fmt.Sprint(widget["com.apple.security.application-groups"]) != "[group.widget]" {
		t.Fatalf("widget entitlements = %v", widget)
	}
	// The domains requested for the main app must not leak into the extension.
	if fmt.Sprint(widget["com.apple.developer.associated-domains"]) != "*" {
		t.Fatalf("widget associated domains = %v", widget["com.apple.developer.associated-domains"])
	}
}

func TestSignPlanCreatesMissingProfileForNestedBundleOnly(t *testing.T) {
	_, plan := widgetPlan(t)
	signer, _, provider, log := newSigner(t)
	provider.existing[testMainID] = []provisioningProfile{testProfile(testMainID)}
	if err := signer.signPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(provider.created) != fmt.Sprint([]string{testMainID + ".widget"}) {
		t.Fatalf("created = %v", provider.created)
	}
	if !strings.Contains(log.String(), "Created a IOS_APP_STORE provisioning profile for "+testMainID+".widget") {
		t.Fatalf("log = %s", log.String())
	}
}

func TestSignPlanRejectsBadProfiles(t *testing.T) {
	expired := testProfile(testMainID + ".widget")
	expired.ExpirationDate = time.Now().Add(-time.Hour)
	otherCert := testProfile(testMainID + ".widget")
	otherCert.DeveloperCertificates = [][]byte{[]byte("someone else")}
	debug := testProfile(testMainID + ".widget")
	debug.Entitlements["get-task-allow"] = true
	otherTeam := testProfile(testMainID + ".widget")
	otherTeam.TeamIdentifier = []string{"OTHERTEAM1"}
	adHocShape := testProfile(testMainID + ".widget")
	adHocShape.ProvisionedDevices = []string{"udid"}
	// A profile for the main app must never satisfy the extension.
	wrongBundle := testProfile(testMainID)
	for name, bad := range map[string]provisioningProfile{
		"expired": expired, "other certificate": otherCert, "debug": debug, "other team": otherTeam,
		"device list on app store": adHocShape, "main profile for extension": wrongBundle,
	} {
		t.Run(name, func(t *testing.T) {
			_, plan := widgetPlan(t)
			signer, tools, provider, _ := newSigner(t)
			provider.existing[testMainID] = []provisioningProfile{testProfile(testMainID)}
			provider.existing[testMainID+".widget"] = []provisioningProfile{bad}
			err := signer.signPlan(context.Background(), plan)
			if name == "other team" {
				if err == nil {
					t.Fatal("profile of another team was accepted")
				}
				return
			}
			// The unusable profile is skipped and a fresh one bound to the exact
			// identifier is created instead.
			if err != nil || fmt.Sprint(provider.created) != fmt.Sprint([]string{testMainID + ".widget"}) {
				t.Fatalf("err = %v, created = %v", err, provider.created)
			}
			_ = tools
		})
	}
}

func TestSignPlanFailsWhenNestedProfileUnavailable(t *testing.T) {
	_, plan := widgetPlan(t)
	signer, tools, provider, _ := newSigner(t)
	provider.existing[testMainID] = []provisioningProfile{testProfile(testMainID)}
	provider.missing[testMainID+".widget"] = true
	err := signer.signPlan(context.Background(), plan)
	if err == nil || !strings.Contains(err.Error(), "PlugIns/Widget.appex") {
		t.Fatalf("err = %v", err)
	}
	if tools.verified != "" {
		t.Fatal("verification ran after a failure")
	}
}

func TestSignPlanUsesProtectedFallbackProfiles(t *testing.T) {
	f := newFixture(t)
	f.appex("PlugIns/Widget.appex", testMainID+".widget", "x")
	plan, _ := f.plan()
	signer, tools, provider, log := newSigner(t)
	provider.missing[testMainID], provider.missing[testMainID+".widget"] = true, true
	for i, id := range []string{testMainID, testMainID + ".widget"} {
		path := filepath.Join(signer.secretsDir, fmt.Sprintf("static-%d.mobileprovision", i))
		tools.profiles[path] = testProfile(id)
		if err := os.WriteFile(path, []byte("profile"), 0o600); err != nil {
			t.Fatal(err)
		}
		signer.static = append(signer.static, path)
	}
	if err := signer.signPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if len(tools.signed) != 2 || !strings.Contains(log.String(), "Trying protected fallback profiles") {
		t.Fatalf("signed %v, log %s", tools.signed, log.String())
	}
}

func TestSignPlanWithoutProviderNeedsAllProfiles(t *testing.T) {
	_, plan := widgetPlan(t)
	signer, tools, _, _ := newSigner(t)
	signer.provider = nil
	path := filepath.Join(signer.secretsDir, "main.mobileprovision")
	_ = os.WriteFile(path, []byte("profile"), 0o600)
	tools.profiles[path] = testProfile(testMainID)
	signer.static = []string{path}
	if err := signer.signPlan(context.Background(), plan); err == nil {
		t.Fatal("extension signed without a profile")
	}
}

func TestSignPlanStopsOnCodesignFailure(t *testing.T) {
	_, plan := widgetPlan(t)
	signer, tools, provider, _ := newSigner(t)
	provider.existing[testMainID] = []provisioningProfile{testProfile(testMainID)}
	provider.existing[testMainID+".widget"] = []provisioningProfile{testProfile(testMainID + ".widget")}
	tools.failOn = "Widget.appex"
	if err := signer.signPlan(context.Background(), plan); err == nil {
		t.Fatal("expected failure")
	}
	if tools.verified != "" || len(tools.signed) != 1 {
		t.Fatalf("signed = %v verified = %q", tools.signed, tools.verified)
	}
}

func TestAppGroupMismatchIsNotedWithoutValues(t *testing.T) {
	_, plan := widgetPlan(t)
	signer, _, provider, log := newSigner(t)
	provider.existing[testMainID] = []provisioningProfile{testProfile(testMainID, "group.secret.name")}
	provider.existing[testMainID+".widget"] = []provisioningProfile{testProfile(testMainID + ".widget")}
	if err := signer.signPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "App Group") || strings.Contains(log.String(), "group.secret.name") {
		t.Fatalf("log = %q", log.String())
	}
}

func TestDistributionEntitlementsDoesNotMutateProfile(t *testing.T) {
	profile := map[string]any{"application-identifier": "T.a"}
	out := distributionEntitlements(profile, "T")
	if len(profile) != 1 || out["get-task-allow"] != false || out["com.apple.developer.team-identifier"] != "T" {
		t.Fatalf("profile = %v out = %v", profile, out)
	}
}

func zipApp(t *testing.T, app string) string {
	t.Helper()
	ipa := filepath.Join(t.TempDir(), "App.ipa")
	out, err := os.Create(ipa)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(out)
	root := filepath.Dir(app)
	err = filepath.WalkDir(app, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := "Payload/" + filepath.ToSlash(rel(root, path))
		header := &zip.FileHeader{Name: name, Method: zip.Deflate, CreatorVersion: 3 << 8}
		if entry.IsDir() {
			header.Name += "/"
			header.SetMode(os.ModeDir | 0o755)
			_, err = writer.CreateHeader(header)
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		header.SetMode(0o644)
		w, err := writer.CreateHeader(header)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	})
	if err != nil || writer.Close() != nil || out.Close() != nil {
		t.Fatal("build test IPA")
	}
	return ipa
}

func TestPlanSigningFromIPAWithoutCredentials(t *testing.T) {
	f := newFixture(t)
	f.appex("PlugIns/Widget.appex", testMainID+".widget", "com.apple.widgetkit-extension")
	var out bytes.Buffer
	if err := PlanSigning(&PlanSigningOptions{IPAPath: zipApp(t, f.app), ExpectedBundleID: testMainID}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Provisioning profiles required: 2") {
		t.Fatalf("output = %s", out.String())
	}
	if err := PlanSigning(&PlanSigningOptions{IPAPath: zipApp(t, f.app), ExpectedBundleID: "com.other"}, &out); err == nil {
		t.Fatal("unexpected bundle identifier accepted")
	}
	if err := PlanSigning(&PlanSigningOptions{IPAPath: "relative.ipa"}, &out); err == nil {
		t.Fatal("relative path accepted")
	}
}

func TestValidateTrustedApplicationAcceptsPlannedBundlesOnly(t *testing.T) {
	f := newFixture(t)
	f.file("App")
	f.appex("PlugIns/Widget.appex", testMainID+".widget", "com.apple.widgetkit-extension")
	if err := validateTrustedApplication(f.app); err != nil {
		t.Fatalf("widget app rejected: %v", err)
	}
	f.appex("PlugIns/Evil.appex", "com.attacker.app", "com.apple.widgetkit-extension")
	if err := validateTrustedApplication(f.app); err == nil {
		t.Fatal("foreign extension identifier accepted by trusted packaging")
	}
}
