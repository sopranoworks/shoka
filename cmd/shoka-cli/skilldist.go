package main

import (
	"path/filepath"
	"strings"

	"github.com/sopranoworks/skilldist"
)

// DefaultSkillsRepo is the FIXED source of Shoka's bundled skills: the project's
// own public repository, whose tracked skills/ subtree holds them. It is passed to
// the reusable skilldist library as Config.Source. The public project-repo URL is
// not a deployment detail, so baking it as the default is within the
// confidentiality rule (which concerns deployment topology, not the public source).
const DefaultSkillsRepo = "https://github.com/sopranoworks/shoka.git"

// skillSignature is Shoka's distributor identity stamped into / verified against
// each managed skill's .skill-meta.yaml. It is the ownership boundary the library's
// Verify/Prune use, replacing the old (unenforced) shoka- name-prefix assumption.
const skillSignature = "shoka"

// resolveRepo returns the override when non-empty, else the fixed default source.
func resolveRepo(override string) string {
	if strings.TrimSpace(override) != "" {
		return override
	}
	return DefaultSkillsRepo
}

// skilldistConfig builds the injected skilldist.Config from Shoka's fixed values
// plus the resolved runtime convention path. This is the entire Shoka-specific
// surface: the source repo, the cache app name, the convention path, and the
// signature — everything else lives in the reusable library.
func skilldistConfig(runtime string, global bool, repoOverride string) (skilldist.Config, error) {
	conv, err := skillsConventionDir(runtime, global)
	if err != nil {
		return skilldist.Config{}, err
	}
	return skilldist.Config{
		Source:         resolveRepo(repoOverride),
		AppName:        "shoka",
		ConventionPath: conv,
		Signature:      skillSignature,
		SkillsSubdir:   "skills",
		SkillMarker:    "SKILL.md",
	}, nil
}

// skillsConventionDir resolves the skills directory for a runtime — the install
// destination parent, <returned>/<name>/. --global selects the user-level
// location. It builds on the shared conventionDir resolver so the runtime→path
// mapping is defined once:
//
//	claude -> .claude/skills   (~/.claude/skills with --global)
//	gemini -> .gemini/skills   (~/.gemini/skills with --global)
func skillsConventionDir(runtime string, global bool) (string, error) {
	base, err := conventionDir(runtime, global)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "skills"), nil
}
