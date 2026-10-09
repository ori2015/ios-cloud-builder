package projectdetect

import "strings"

// INISection is one [header] and the text under it.
type INISection struct{ Header, Body string }

// SplitINI splits Godot's INI-like files into [section] headers and bodies.
func SplitINI(text string) []INISection {
	var sections []INISection
	current := -1
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			sections = append(sections, INISection{Header: trimmed})
			current = len(sections) - 1
			continue
		}
		if current >= 0 {
			sections[current].Body += line + "\n"
		}
	}
	return sections
}

// INIValue returns the unquoted value of key in an INI section body, or "".
func INIValue(body, key string) string {
	for _, line := range strings.Split(body, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && strings.TrimSpace(k) == key {
			return strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return ""
}
