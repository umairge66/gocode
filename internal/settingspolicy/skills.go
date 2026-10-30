package settingspolicy

// skillNamePrefix is the namespace prefix for built-in skill identifiers.
const skillNamePrefix = "gocode-skills:"

// NormalizeSkillName ensures a skill identifier carries the "gocode-skills:"
// prefix, returning names that already carry it unchanged.
func NormalizeSkillName(name string) string {
	if len(name) >= len(skillNamePrefix) && name[:len(skillNamePrefix)] == skillNamePrefix {
		return name
	}
	return skillNamePrefix + name
}
