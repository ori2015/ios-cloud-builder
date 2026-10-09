package runner

import (
	"strings"
	"testing"
)

func TestPlanWebBuild(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string
		want    webBuildPlan
		wantErr string
	}{
		{"build script and json webDir", map[string]string{
			"package.json": `{"scripts":{"build":"vite build"}}`, "capacitor.config.json": `{"appId":"a.b","webDir":"dist"}`,
		}, webBuildPlan{Script: "build", WebDir: "dist"}, ""},
		{"build:web when build is absent", map[string]string{
			"package.json": `{"scripts":{"build:web":"x","lint":"y"}}`, "capacitor.config.ts": "export default { webDir: './www' }",
		}, webBuildPlan{Script: "build:web", WebDir: "www"}, ""},
		{"build wins over build:web", map[string]string{
			"package.json": `{"scripts":{"build":"a","build:web":"b"}}`, "capacitor.config.ts": "webDir: 'dist'",
		}, webBuildPlan{Script: "build", WebDir: "dist"}, ""},
		{"committed assets without a script", map[string]string{
			"package.json": `{}`, "capacitor.config.json": `{"webDir":"www"}`, "www/index.html": "<html>",
		}, webBuildPlan{WebDir: "www"}, ""},
		{"no script and nothing committed says what to add", map[string]string{
			"package.json": `{"scripts":{"lint":"x"}}`, "capacitor.config.json": `{"webDir":"dist"}`,
		}, webBuildPlan{}, `add a build script that writes the web app to dist`},
		{"empty script is not a script", map[string]string{
			"package.json": `{"scripts":{"build":"  "}}`, "capacitor.config.json": `{"webDir":"dist"}`,
		}, webBuildPlan{}, `no "build" or "build:web" script`},
		{"config without webDir", map[string]string{
			"package.json": `{"scripts":{"build":"x"}}`, "capacitor.config.ts": "export default { appId: 'a.b' }",
		}, webBuildPlan{}, "has no webDir"},
		{"webDir escaping the app", map[string]string{
			"package.json": `{"scripts":{"build":"x"}}`, "capacitor.config.json": `{"webDir":"../dist"}`,
		}, webBuildPlan{}, "relative folder inside the app"},
		{"no capacitor config", map[string]string{"package.json": `{"scripts":{"build":"x"}}`}, webBuildPlan{}, "no capacitor.config"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for name, contents := range tc.files {
				writeFile(t, root, name, contents)
			}
			got, err := planWebBuild(root)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

func TestVerifyWebAssets(t *testing.T) {
	root := t.TempDir()
	plan := webBuildPlan{Script: "build", WebDir: "dist"}
	if err := plan.verifyWebAssets(root); err == nil || !strings.Contains(err.Error(), "dist/index.html") {
		t.Fatalf("missing output not reported: %v", err)
	}
	writeFile(t, root, "dist/index.html", "<html>")
	if err := plan.verifyWebAssets(root); err != nil {
		t.Fatal(err)
	}
}

func TestScriptCommandFollowsLockfile(t *testing.T) {
	for lock, want := range map[string]string{"pnpm-lock.yaml": "corepack pnpm run build", "yarn.lock": "corepack yarn run build", "package-lock.json": "npm run build", "": "npm run build"} {
		root := t.TempDir()
		if lock != "" {
			writeFile(t, root, lock, "x")
		}
		program, args := scriptCommand(root, "build")
		if got := program + " " + strings.Join(args, " "); got != want {
			t.Errorf("lock %q: %s, want %s", lock, got, want)
		}
	}
}

func TestExplainPubGetFailure(t *testing.T) {
	lock := "packages:\n  test_api:\n    dependency: transitive\n    description:\n      name: test_api\n    source: hosted\n    version: \"0.6.1\"\n  path:\n    dependency: direct main\n    source: hosted\n    version: \"1.8.3\"\nsdks:\n  dart: \">=3.0.0 <4.0.0\"\n"
	output := "Because every version of flutter_test from sdk depends on test_api 0.7.2 and generic depends on test_api 0.6.1, flutter_test from sdk is incompatible with generic.\nSo, because app depends on flutter_test from sdk, version solving failed.\nThe current Flutter SDK version is 3.27.1.\n"
	got := explainPubGetFailure(output, lock)
	for _, want := range []string{"Flutter 3.27.1", "test_api 0.6.1", ".flutter-version"} {
		if !strings.Contains(got, want) {
			t.Errorf("explanation missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "path 1.8.3") {
		t.Errorf("unrelated package reported: %s", got)
	}
	if got := explainPubGetFailure("Failed host lookup: 'pub.dev'", lock); got != "" {
		t.Errorf("network failure explained as a solver conflict: %s", got)
	}
}
