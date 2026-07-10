package utils

import "strings"

// IsValidName checks if a name (namespace or project name) is valid. Allowed
// characters: alphanumeric, hyphens, underscores, and dots — matching the
// practical GitHub repository naming set. Additional rules:
//   - not empty, max 100 characters
//   - not "." or ".." (filesystem reserved)
//   - must not start with "." (dot-prefixed dirs are Shoka-internal)
//   - must not end with ".git" (Git convention)
func IsValidName(name string) bool {
	if name == "" || len(name) > 100 {
		return false
	}
	if name == "." || name == ".." {
		return false
	}
	if name[0] == '.' {
		return false
	}
	if strings.HasSuffix(name, ".git") {
		return false
	}
	for _, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}
