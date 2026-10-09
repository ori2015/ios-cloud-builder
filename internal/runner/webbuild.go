package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// webBuildScripts are the package.json scripts that produce the web app, in
// the order they are looked for.
var webBuildScripts = []string{"build", "build:web"}

var capacitorWebDirRe = regexp.MustCompile(`webDir\s*:\s*['"]([^'"]+)['"]`)

// webBuildPlan says how a Capacitor or Ionic project's web assets are produced
// before `cap sync`.
type webBuildPlan struct {
	Script string // package.json script to run; empty when the assets are committed
	WebDir string // folder, relative to the app root, that must hold index.html afterwards
}

// planWebBuild reads package.json and the Capacitor config. It fails with the
// exact thing to add rather than letting `cap sync` copy nothing.
func planWebBuild(appRoot string) (webBuildPlan, error) {
	webDir, err := capacitorWebDir(appRoot)
	if err != nil {
		return webBuildPlan{}, err
	}
	plan := webBuildPlan{WebDir: webDir}
	data, readErr := os.ReadFile(filepath.Join(appRoot, "package.json"))
	if readErr == nil {
		var pkg struct {
			Scripts map[string]string `json:"scripts"`
		}
		if json.Unmarshal(data, &pkg) == nil {
			for _, name := range webBuildScripts {
				if strings.TrimSpace(pkg.Scripts[name]) != "" {
					plan.Script = name
					break
				}
			}
		}
	}
	if plan.Script == "" && !exists(filepath.Join(appRoot, filepath.FromSlash(webDir), "index.html")) {
		return plan, fmt.Errorf(`package.json has no "%s" script and %s/index.html is not committed; add a build script that writes the web app to %s`, strings.Join(webBuildScripts, `" or "`), webDir, webDir)
	}
	return plan, nil
}

// verifyWebAssets checks the build produced what `cap sync` will copy.
func (p webBuildPlan) verifyWebAssets(appRoot string) error {
	if !exists(filepath.Join(appRoot, filepath.FromSlash(p.WebDir), "index.html")) {
		return fmt.Errorf("%s/index.html is missing after the web build; set webDir in the Capacitor config to the folder your build writes to", p.WebDir)
	}
	return nil
}

func capacitorWebDir(appRoot string) (string, error) {
	for _, name := range []string{"capacitor.config.json", "capacitor.config.ts", "capacitor.config.js"} {
		data, err := os.ReadFile(filepath.Join(appRoot, name))
		if err != nil {
			continue
		}
		var cfg struct {
			WebDir string `json:"webDir"`
		}
		webDir := ""
		if json.Unmarshal(data, &cfg) == nil && cfg.WebDir != "" {
			webDir = cfg.WebDir
		} else if m := capacitorWebDirRe.FindSubmatch(data); m != nil {
			webDir = string(m[1])
		}
		if webDir == "" {
			return "", fmt.Errorf("%s has no webDir; set webDir to the folder holding the built web app", name)
		}
		webDir = strings.TrimPrefix(webDir, "./")
		if validateRelativePath(webDir) != nil {
			return "", fmt.Errorf("%s webDir %q must be a relative folder inside the app", name, webDir)
		}
		return webDir, nil
	}
	return "", fmt.Errorf("no capacitor.config.json, .ts or .js found in the app folder")
}

// scriptCommand returns the command that runs a package.json script with the
// package manager the project's lockfile selects.
func scriptCommand(root, script string) (program string, args []string) {
	switch {
	case exists(filepath.Join(root, "pnpm-lock.yaml")):
		return "corepack", []string{"pnpm", "run", script}
	case exists(filepath.Join(root, "yarn.lock")):
		return "corepack", []string{"yarn", "run", script}
	default:
		return "npm", []string{"run", script}
	}
}
