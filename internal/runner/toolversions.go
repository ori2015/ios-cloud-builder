package runner

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ToolVersions are the toolchain versions a project pins in its own files.
// Every value is validated to a plain version number (or "lts/*" for Node) so it
// is safe to pass to the workflow as an output; an empty field means "not pinned".
type ToolVersions struct {
	Flutter string
	Node    string
	Xcode   string
}

var (
	exactVersionRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	looseVersionRe = regexp.MustCompile(`^\d+(\.\d+){0,2}$`)
)

// ToolVersionsAt is ReadToolVersions for an app in the repository-relative
// folder appPath (empty or "." for the checkout root).
func ToolVersionsAt(sourceRoot, appPath string) (ToolVersions, error) {
	sourceRoot, err := filepath.EvalSymlinks(sourceRoot)
	if err != nil {
		return ToolVersions{}, err
	}
	appRoot := sourceRoot
	if appPath != "" && appPath != "." {
		if err := validateRelativePath(appPath); err != nil {
			return ToolVersions{}, err
		}
		resolved, err := filepath.EvalSymlinks(filepath.Join(sourceRoot, filepath.FromSlash(appPath)))
		if err != nil || !pathWithin(sourceRoot, resolved) {
			return ToolVersions{}, os.ErrNotExist
		}
		appRoot = resolved
	}
	return ReadToolVersions(sourceRoot, appRoot), nil
}

// ReadToolVersions reads version pins from appRoot, then from the checkout
// root, taking the first valid value for each tool:
//
//	Flutter: .flutter-version, .fvmrc, .fvm/fvm_config.json
//	Node:    .nvmrc, .node-version, package.json engines.node
//	Xcode:   .xcode-version
//
// pubspec.lock is deliberately not a pin: its sdks entry is the lower bound of
// the pubspec environment constraint (often a years-old release), not the
// version the lock was resolved with.
func ReadToolVersions(sourceRoot, appRoot string) ToolVersions {
	dirs := []string{appRoot}
	if appRoot != sourceRoot {
		dirs = append(dirs, sourceRoot)
	}
	var out ToolVersions
	for _, dir := range dirs {
		if out.Flutter == "" {
			out.Flutter = firstValid(exactVersionRe,
				firstLine(filepath.Join(dir, ".flutter-version")),
				jsonString(filepath.Join(dir, ".fvmrc"), "flutter"),
				jsonString(filepath.Join(dir, ".fvm", "fvm_config.json"), "flutterSdkVersion"))
		}
		if out.Node == "" {
			out.Node = nodeVersion(dir)
		}
		if out.Xcode == "" {
			out.Xcode = firstValid(looseVersionRe, firstLine(filepath.Join(dir, ".xcode-version")))
		}
	}
	return out
}

func nodeVersion(dir string) string {
	for _, name := range []string{".nvmrc", ".node-version"} {
		value := strings.TrimPrefix(firstLine(filepath.Join(dir, name)), "v")
		if value == "lts/*" || looseVersionRe.MatchString(value) {
			return value
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return ""
	}
	var pkg struct {
		Engines struct {
			Node string `json:"node"`
		} `json:"engines"`
	}
	if json.Unmarshal(data, &pkg) != nil {
		return ""
	}
	return nodeMajorFor(pkg.Engines.Node)
}

// nodeMajorFor picks the first major (20, the long-standing default, then 22, 24, 18, 16, 14) that satisfies a semver range
// (comparisons are made on the major version only), or "" when the range is
// empty, unparseable or satisfied by none of them.
func nodeMajorFor(rng string) string {
	rng = strings.TrimSpace(rng)
	if rng == "" {
		return ""
	}
	for _, major := range []int{20, 22, 24, 18, 16, 14} {
		if satisfiesMajor(rng, major) {
			return strconv.Itoa(major)
		}
	}
	return ""
}

var comparatorRe = regexp.MustCompile(`^(>=|<=|>|<|=|\^|~)?\s*v?(\d+|\*|x)(?:\.(\d+|\*|x))?(?:\.(\d+|\*|x))?(?:-.*)?$`)
var opSpaceRe = regexp.MustCompile(`(>=|<=|>|<|=|\^|~)\s+`)

func satisfiesMajor(rng string, major int) bool {
	for _, alternative := range strings.Split(rng, "||") {
		ok, parsed := true, false
		for _, token := range strings.Fields(opSpaceRe.ReplaceAllString(strings.TrimSpace(alternative), "$1")) {
			m := comparatorRe.FindStringSubmatch(token)
			if m == nil {
				return false // hyphen ranges and other forms: do not guess
			}
			parsed = true
			if m[2] == "*" || m[2] == "x" {
				continue
			}
			bound, _ := strconv.Atoi(m[2])
			switch m[1] {
			case ">=":
				ok = ok && major >= bound
			case ">":
				ok = ok && major > bound
			case "<=":
				ok = ok && major <= bound
			case "<":
				ok = ok && major < bound
			default: // =, ^, ~ or a bare/partial version
				ok = ok && major == bound
			}
		}
		if parsed && ok {
			return true
		}
	}
	return false
}

func firstLine(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 4096), 4096)
	if scanner.Scan() {
		return strings.TrimSpace(scanner.Text())
	}
	return ""
}

func jsonString(path, key string) string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 1<<20 {
		return ""
	}
	var fields map[string]any
	if json.Unmarshal(data, &fields) != nil {
		return ""
	}
	value, _ := fields[key].(string)
	return strings.TrimSpace(value)
}

func firstValid(pattern *regexp.Regexp, candidates ...string) string {
	for _, candidate := range candidates {
		if pattern.MatchString(candidate) {
			return candidate
		}
	}
	return ""
}
