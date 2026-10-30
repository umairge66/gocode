package builtincmds

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// /team-onboarding: scans the user's LOCAL transcripts of the current project
// for the last 30 days (slash commands used, MCP servers called, a short
// descriptor per session), and asks the model to write an ONBOARDING.md guide
// for teammates from that usage data.
// Everything stays local: the guide is not uploaded or shared and no usage
// events are recorded.

// TeamOnboardingWindowDays is how many days of transcripts are scanned.
const TeamOnboardingWindowDays = 30

// TeamOnboardingAllowedTools are the tools the command's turn may use without
// asking. There is no share tool.
var TeamOnboardingAllowedTools = []string{"Edit(ONBOARDING.md)", "Bash(ls *)"}

// TeamOnboardingEffort is the command's effort override.
const TeamOnboardingEffort = "low"

const (
	// maxOnboardingFileSize skips huge transcripts.
	maxOnboardingFileSize = 52428800
	// maxFirstMessage caps a session's first message.
	maxFirstMessage = 200
	// maxSessionDescriptors caps the descriptor list.
	maxSessionDescriptors = 60
)

// Transcript-line probes and extractors.
var (
	commandNameRE   = regexp.MustCompile(`<command-name>/([\w:-]+)</command-name>`)
	mcpToolNameRE   = regexp.MustCompile(`"name":"mcp__([^"]+?)__([^"]+)"`)
	customTitleRE   = regexp.MustCompile(`"customTitle":"([^"]+)"`)
	prNumberRE      = regexp.MustCompile(`"prNumber":(\d+)`)
	userStringMsgRE = regexp.MustCompile(`"role":"user"[^}]*"content":"([^"]+)"`)
	// Transcripts always store content as a block array, so a user turn's
	// leading text block is read too (not only string content).
	userBlockMsgRE = regexp.MustCompile(`"role":"user","content":\[\{"type":"text","text":"([^"]+)"`)
)

const (
	probeCommandContent = `"content":"<command-name>/`
	probeCommandMessage = `"content":"<command-message>`
	probeToolUse        = `"type":"tool_use"`
	probeCustomTitle    = `"type":"custom-title"`
	probePRLink         = `"type":"pr-link"`
	probeUserRole       = `"role":"user"`
)

// sessionDescriptor describes one session for the work-type classification.
type sessionDescriptor struct {
	PRNumbers    []int  `json:"prNumbers"`
	Title        string `json:"title,omitempty"`
	FirstMessage string `json:"firstMessage,omitempty"`
}

// onboardingScan is the transcript scan result.
type onboardingScan struct {
	slashCommandCounts map[string]int
	mcpServerCounts    map[string]int
	sessionDescriptors []sessionDescriptor
	sessionFileCount   int
}

// scanOnboardingUsage reads the .jsonl transcripts in dir modified within the
// window.
func scanOnboardingUsage(dir string, windowDays int, now time.Time) (onboardingScan, error) {
	r := onboardingScan{slashCommandCounts: map[string]int{}, mcpServerCounts: map[string]int{}}
	cutoff := now.Add(-time.Duration(windowDays) * 24 * time.Hour)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return r, nil
		}
		return r, err
	}
	var files []string
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		p := filepath.Join(dir, e.Name())
		st, err := os.Stat(p)
		if err != nil || !st.Mode().IsRegular() || st.ModTime().Before(cutoff) || st.Size() > maxOnboardingFileSize {
			continue
		}
		files = append(files, p)
	}
	for _, p := range files {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		r.sessionFileCount++
		d := sessionDescriptor{PRNumbers: []int{}}
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 0, 64*1024), maxOnboardingFileSize)
		for sc.Scan() {
			line := sc.Text()
			if len(line) < 10 {
				continue
			}
			if strings.Contains(line, probeCommandContent) || strings.Contains(line, probeCommandMessage) {
				for _, m := range commandNameRE.FindAllStringSubmatch(line, -1) {
					r.slashCommandCounts[m[1]]++
				}
			}
			if strings.Contains(line, probeToolUse) && strings.Contains(line, `"name":"mcp__`) {
				for _, m := range mcpToolNameRE.FindAllStringSubmatch(line, -1) {
					r.mcpServerCounts[m[1]]++
				}
			}
			if strings.Contains(line, probeCustomTitle) {
				if m := customTitleRE.FindStringSubmatch(line); m != nil {
					d.Title = m[1]
				}
			}
			if strings.Contains(line, probePRLink) {
				if m := prNumberRE.FindStringSubmatch(line); m != nil {
					n, _ := strconv.Atoi(m[1])
					if !containsInt(d.PRNumbers, n) {
						d.PRNumbers = append(d.PRNumbers, n)
					}
				}
			}
			if d.FirstMessage == "" && strings.Contains(line, probeUserRole) && !strings.Contains(line, probeCommandContent) {
				var m []string
				if !strings.Contains(line, `"content":[`) {
					m = userStringMsgRE.FindStringSubmatch(line)
				} else {
					m = userBlockMsgRE.FindStringSubmatch(line)
				}
				if m != nil {
					msg := strings.ReplaceAll(strings.ReplaceAll(m[1], `\n`, " "), `\"`, `"`)
					if len(msg) > 3 && !strings.HasPrefix(msg, "<") {
						d.FirstMessage = truncateRunes(msg, maxFirstMessage)
					}
				}
			}
		}
		if d.Title != "" || len(d.PRNumbers) > 0 || d.FirstMessage != "" {
			r.sessionDescriptors = append(r.sessionDescriptors, d)
		}
	}
	if len(r.sessionDescriptors) > maxSessionDescriptors {
		score := func(d sessionDescriptor) int {
			s := 0
			if d.Title != "" {
				s += 2
			}
			if len(d.PRNumbers) > 0 {
				s++
			}
			return s
		}
		sort.SliceStable(r.sessionDescriptors, func(i, j int) bool {
			return score(r.sessionDescriptors[i]) > score(r.sessionDescriptors[j])
		})
		r.sessionDescriptors = r.sessionDescriptors[:maxSessionDescriptors]
	}
	return r, nil
}

func containsInt(xs []int, n int) bool {
	for _, x := range xs {
		if x == n {
			return true
		}
	}
	return false
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// TeamOnboardingOptions are the inputs of TeamOnboardingPrompt.
type TeamOnboardingOptions struct {
	// TranscriptDir is the current project's transcript directory.
	TranscriptDir string
	// Cwd is the working directory (its .mcp.json and git repo are read).
	Cwd string
	// Now is the clock (nil: time.Now).
	Now func() time.Time
	// Git runs a git command in Cwd and returns its trimmed stdout ("" on
	// failure). nil runs the real git binary.
	Git func(ctx context.Context, args ...string) string
}

type onboardingCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type onboardingMCPServer struct {
	Name      string `json:"name"`
	CallCount int    `json:"callCount"`
	URLOrigin string `json:"urlOrigin,omitempty"`
}

// onboardingUsage is the usage-data JSON, in its fixed key order.
type onboardingUsage struct {
	GeneratedBy        string                `json:"generatedBy,omitempty"`
	CurrentRepo        string                `json:"currentRepo"`
	WindowDays         int                   `json:"windowDays"`
	SessionCount       int                   `json:"sessionCount"`
	SlashCommands      []onboardingCount     `json:"slashCommands"`
	MCPServers         []onboardingMCPServer `json:"mcpServers"`
	SessionDescriptors []sessionDescriptor   `json:"sessionDescriptors"`
}

// TeamOnboardingPrompt builds the /team-onboarding prompt from the local usage
// data.
func TeamOnboardingPrompt(ctx context.Context, opts TeamOnboardingOptions) (string, error) {
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	git := opts.Git
	if git == nil {
		git = func(ctx context.Context, args ...string) string {
			cmd := exec.CommandContext(ctx, "git", args...)
			cmd.Dir = opts.Cwd
			out, err := cmd.Output()
			if err != nil {
				return ""
			}
			return strings.TrimSpace(string(out))
		}
	}
	days := TeamOnboardingWindowDays
	scan, err := scanOnboardingUsage(opts.TranscriptDir, days, now())
	if err != nil {
		return "", err
	}
	usage := onboardingUsage{
		GeneratedBy:        git(ctx, "config", "user.name"),
		WindowDays:         days,
		SessionCount:       scan.sessionFileCount,
		SlashCommands:      []onboardingCount{},
		MCPServers:         []onboardingMCPServer{},
		SessionDescriptors: scan.sessionDescriptors,
	}
	if usage.SessionDescriptors == nil {
		usage.SessionDescriptors = []sessionDescriptor{}
	}
	usage.CurrentRepo = repoSlug(git(ctx, "remote", "get-url", "origin"))
	if usage.CurrentRepo == "" {
		usage.CurrentRepo = filepath.Base(opts.Cwd)
	}
	for name, n := range scan.slashCommandCounts {
		usage.SlashCommands = append(usage.SlashCommands, onboardingCount{Name: "/" + name, Count: n})
	}
	// Sorted by count, most first; ties by name, since Go maps are
	// unordered.
	sort.SliceStable(usage.SlashCommands, func(i, j int) bool {
		a, b := usage.SlashCommands[i], usage.SlashCommands[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.Name < b.Name
	})
	servers := projectMCPServers(opts.Cwd)
	for name, n := range scan.mcpServerCounts {
		s := onboardingMCPServer{Name: name, CallCount: n}
		if cfg, ok := servers[name].(map[string]any); ok {
			if u, ok := cfg["url"].(string); ok {
				s.URLOrigin = urlOrigin(u)
			}
		}
		usage.MCPServers = append(usage.MCPServers, s)
	}
	sort.SliceStable(usage.MCPServers, func(i, j int) bool {
		a, b := usage.MCPServers[i], usage.MCPServers[j]
		if a.CallCount != b.CallCount {
			return a.CallCount > b.CallCount
		}
		return a.Name < b.Name
	})
	// JSON.stringify(…, null, 2): no HTML escaping, no trailing newline.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(usage); err != nil {
		return "", err
	}
	data := bytes.TrimRight(buf.Bytes(), "\n")
	prompt := strings.ReplaceAll(teamOnboardingPromptTemplate, "{{WINDOW_DAYS}}", strconv.Itoa(days))
	prompt = strings.ReplaceAll(prompt, "{{GUIDE_TEMPLATE}}", teamOnboardingGuideTemplate)
	prompt = strings.ReplaceAll(prompt, "{{USAGE_DATA}}", string(data))
	return prompt, nil
}

// projectMCPServers reads the mcpServers object of <cwd>/.mcp.json (empty on
// any failure).
func projectMCPServers(cwd string) map[string]any {
	data, err := os.ReadFile(filepath.Join(cwd, ".mcp.json"))
	if err != nil {
		return map[string]any{}
	}
	var doc struct {
		MCPServers map[string]any `json:"mcpServers"`
	}
	if json.Unmarshal(data, &doc) != nil || doc.MCPServers == nil {
		return map[string]any{}
	}
	return doc.MCPServers
}

// urlOrigin is scheme://host[:port] of a URL ("" when unparsable).
func urlOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

var (
	scpRemoteRE = regexp.MustCompile(`^git@([^:/@]+):(.+?)(?:\.git)?$`)
	urlRemoteRE = regexp.MustCompile(`^(?:https?|ssh)://(?:[^@/?#]*@)?([^/?#@]+)/(.+?)(?:\.git)?$`)
)

// repoSlug turns a git remote URL into "host/owner/repo" (lower-cased), or ""
// for an empty or local remote.
func repoSlug(remote string) string {
	n := strings.TrimSpace(remote)
	if n == "" || strings.HasPrefix(n, "/") || strings.HasPrefix(n, ".") || strings.HasPrefix(n, "file:") {
		return ""
	}
	if m := scpRemoteRE.FindStringSubmatch(n); m != nil {
		if strings.Contains(m[2], "..") {
			return ""
		}
		return strings.ToLower(m[1] + "/" + m[2])
	}
	if m := urlRemoteRE.FindStringSubmatch(n); m != nil {
		if strings.Contains(m[2], "..") {
			return ""
		}
		return strings.ToLower(m[1] + "/" + m[2])
	}
	return ""
}
