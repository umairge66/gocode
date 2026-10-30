package updater

import (
	"errors"
	"fmt"
	"strings"
)

// DefaultRepo is the build-time default release repository ("owner/repo").
// It is empty in source; release builds set it with
//
//	go build -ldflags "-X github.com/umairge66/gocode/internal/updater.DefaultRepo=owner/repo"
var DefaultRepo = ""

// RepoEnvVar overrides the release repository at run time.
const RepoEnvVar = "GOCODE_UPDATE_REPO"

// RepoSettingsKey is the settings key the CLI reads and passes to ResolveRepo.
const RepoSettingsKey = "updateRepo"

// ErrRepoNotConfigured is returned when no release repository is configured
// anywhere. No network call is made in that case.
var ErrRepoNotConfigured = errors.New("update source not configured: set GOCODE_UPDATE_REPO=owner/repo")

// RepoSource names where the resolved repository came from.
type RepoSource string

const (
	RepoFromEnv      RepoSource = "env"
	RepoFromSettings RepoSource = "settings"
	RepoFromBuild    RepoSource = "build"
)

// ResolveRepo picks the release repository with precedence
// $GOCODE_UPDATE_REPO > settings "updateRepo" > DefaultRepo, and validates the
// winner's "owner/repo" shape. An invalid higher-precedence value is an error
// (it is not silently skipped).
func ResolveRepo(getenv func(string) string, settingsRepo string) (string, RepoSource, error) {
	var envRepo string
	if getenv != nil {
		envRepo = strings.TrimSpace(getenv(RepoEnvVar))
	}
	candidates := []struct {
		val string
		src RepoSource
	}{
		{envRepo, RepoFromEnv},
		{strings.TrimSpace(settingsRepo), RepoFromSettings},
		{strings.TrimSpace(DefaultRepo), RepoFromBuild},
	}
	for _, c := range candidates {
		if c.val == "" {
			continue
		}
		if err := ValidateRepo(c.val); err != nil {
			return "", c.src, fmt.Errorf("%w (from %s)", err, c.src)
		}
		return c.val, c.src, nil
	}
	return "", "", ErrRepoNotConfigured
}

// ValidateRepo checks that repo is "owner/repo" using GitHub's allowed
// characters (alphanumerics, '-', '_', '.'), with no other path segments.
func ValidateRepo(repo string) error {
	if repo == "" {
		return ErrRepoNotConfigured
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || !validRepoPart(owner) || !validRepoPart(name) || name == "." || name == ".." || owner == "." || owner == ".." {
		return fmt.Errorf("updater: invalid repository %q: want owner/repo", repo)
	}
	return nil
}

func validRepoPart(s string) bool {
	if s == "" || len(s) > 100 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}
