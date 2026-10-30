package toolreg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	pathguard "github.com/umairge66/gocode/internal/pathguard"
	"github.com/umairge66/gocode/internal/protocol"
)

// The read-only classifier, table-driven from its command lists and its
// refusals.
func TestIsReadOnlyBashCommand(t *testing.T) {
	readOnly := []string{
		// simple allowlist with any arguments.
		"cat README.md", "head -n 20 main.go", "tail -f log.txt", "wc -l a b", "stat x",
		"strings bin", "hexdump -C f", "od -c f", "nl f", "id", "uname -a", "free -h", "df -h", "du -sh .",
		"locale", "groups", "nproc", "basename a/b", "dirname a/b", "realpath .", "cut -d: -f1 /etc/passwd",
		"paste a b", "tr a-z A-Z", "column -t f", "tac f", "rev f", "fold -w 80 f", "expand f", "unexpand f",
		"fmt f", "comm a b", "cmp a b", "numfmt --to=iec 1024", "readlink -f x", "diff -u a b", "true", "false",
		"sleep 1", "which go", "type ls", "expr 1 + 2", "seq 10", "tsort f", "pr f", "cal", "uptime",
		"docker ps -a", "docker images",
		// exact forms.
		"pwd", "whoami", "alias", "node -v", "node --version", "python3 --version", "ip addr", "gocode --help",
		// ls / cd / echo / printf / find / history / arch / ifconfig.
		"ls", "ls -la", "ls -R src", "cd src", "cd", "echo hello", "echo 'a b' \"c\"", "printf '%s\\n' x",
		"printf 'hello\\n'", "printf '%d\\n' 42",
		"find . -name '*.go'", "find . -type f -name x -print", "find . -name -delete", "find src -maxdepth 2 -type d",
		"history", "history 20", "arch", "ifconfig", "ifconfig eth0",
		// git subcommands with allowlisted flags.
		"git status", "git status --short", "git status -sb", "git log", "git log --oneline -n 5",
		"git log --oneline -20", "git log --format=%H", "git log --pretty=format:%h", "git log --pretty=oneline",
		"git log --since=yesterday --author alice", "git diff", "git diff --cached --stat", "git diff HEAD~1 -- main.go",
		"git show HEAD", "git show --stat HEAD", "git shortlog -sn", "git reflog", "git reflog show",
		"git stash list", "git stash show -p", "git blame -L 1,10 main.go", "git ls-files", "git ls-files -m",
		"git config --get user.name", "git remote", "git remote -v", "git remote show -n origin",
		"git merge-base HEAD main", "git rev-parse HEAD", "git rev-parse --show-toplevel", "git rev-list --count HEAD",
		"git describe --tags", "git cat-file -p HEAD", "git for-each-ref --sort=-committerdate refs/heads",
		"git grep -n foo", "git worktree list", "git tag", "git tag -l 'v1*'", "git branch", "git branch -a",
		"git branch --merged main", "git ls-remote --tags", "git branch --sort=-committerdate",
		// grep / rg / sed / sort / xargs / misc table entries.
		"grep -r x .", "grep -rn TODO src", "grep -A3 foo f", "grep -C 2 foo f", "egrep 'a|b' f", "fgrep x f",
		"rg foo", "rg -n --hidden foo src", "rg -t go foo", "rg -A2 foo",
		"sed -n 1,10p f", "sed -n '5p' f", "sed 's/a/b/g'", "sort f", "sort -rn f", "sort -k 2 -t , f",
		"xargs grep foo", "xargs -0 head -n 1", "file x", "file --mime-type x", "base64 -d f",
		"sha256sum f", "md5sum f", "tree -L 2", "date", "date +%s", "date -u", "hostname", "hostname -f",
		"ps aux", "ps -ef", "pgrep -f node", "lsof -i :8080", "netstat -an", "ss -tlnp", "tput cols",
		"man ls", "help cd", "fd foo", "fd -e go", "docker logs web", "docker inspect web",
		"test -f x", "test 1 -eq 1", "uniq", "uniq -c", "jq '.a' f.json", "jq -r .name package.json",
		// lists and pipelines of read-only commands.
		"ls; pwd", "ls && cat f", "cat f | grep x | wc -l", "git status && git log -n 1", "ls || echo none",
		"cat f 2>/dev/null", "ls 2>&1", "grep x f >/dev/null", "ls &>/dev/null", "cat < f", "ls # comment",
		// safe env prefix and command/builtin wrappers.
		"LANG=C ls", "LC_ALL=C sort f", "NO_COLOR=1 git status", "command ls", "builtin echo x",
		// globs on glob-safe commands.
		"ls *.go", "cat src/*.go", "grep foo *.txt", "wc -l *.md",
	}
	notReadOnly := []string{
		"", "rm -f x", "rm -rf /", "touch x", "mkdir d", "mv a b", "cp a b", "chmod +x f", "ln -s a b",
		// sed writes / executes.
		"sed -i 's/a/b/' f", "sed -i.bak s/a/b/ f", "sed 's/a/b/w out' f", "sed 's/a/b/' f", "sed -n 'w out' f",
		"sed e f", "sed -n '1e date' f", "sed '1d' f", "sed --in-place s/a/b/ f",
		// find actions.
		"find . -delete", "find . -name x -delete", "find . -exec rm {} ;", "find . -execdir ls ;",
		"find . -ok rm {} ;", "find . -fprint out", "find . -fprint0 out", "find . -fls out",
		"find . -fprintf out %p", "find . -files0-from f", "find . -rm",
		// sort / rg / git write vectors.
		"sort -o out f", "sort --output=out f", "rg --pre cat foo", "rg --pre=cat foo", "rg --search-zip foo",
		"git log --output=f", "git -c core.pager=less log", "git -C .. status", "git --exec-path=/x status",
		"git commit -m x", "git push", "git checkout main", "git reset --hard", "git stash", "git stash drop",
		"git branch newbranch", "git branch -D old", "git tag v1", "git tag -d v1", "git reflog expire --all",
		"git reflog delete HEAD@{1}", "git remote add o url", "git remote show origin",
		"git log --format=%G?", "git log --pretty=%GS", "git log --format=myalias", "git config user.name x",
		"git ls-remote origin", "git diff --output=f", "git fetch", "git pull", "git clean -fd",
		// pagers, writers, interpreters, network.
		"less f", "more f", "tee f", "vim f", "nano f", "python3 x.py", "node x.js", "bash -c ls", "sh -c 'ls'",
		"eval ls", "exec ls", "source f", ". f", "curl http://x", "wget http://x", "npm install", "make",
		"xargs rm", "xargs -I {} rm {}", "xargs sh -c ls", "env ls", "sudo ls", "timeout 5 cat f", "nohup ls",
		"awk '{print}' f", "perl -e 1", "dd if=a of=b", "truncate -s0 f", "docker run x", "docker -H tcp://x ps",
		// redirections / substitution / expansion / compound forms.
		"echo x > f", "echo x >> f", "ls > out", "cat f 2> err", "ls >| f", "cat <<EOF\nx\nEOF", "cat <<< x",
		"echo $(rm x)", "echo `rm x`", "ls $(pwd)", "cat <(ls)", "ls >(cat)", "echo $HOME", "ls $DIR",
		"echo \"$HOME\"", "echo ${x}", "ls; rm x", "ls && rm x", "cat f | tee g", "cat f | sh", "(ls)",
		"{ ls; }", "ls &", "ls & rm x", "ls |& cat", "ls ;; ls", "ls &&", "FOO=1 ls", "PATH=/x ls",
		"X=1; ls", "cd /tmp && git status", "cd src && git log", "ls\\ x", "ls \\\n -la",
		"=ls", "ls ~[x]", "echo \x01", "ls -la", "cat ~root/.ssh/id_rsa", "echo {a,b}", "printf -v x y",
		"printf '%*d' x 1", "printf '%s' $(id)", "date -s 2020-01-01", "date 0101", "hostname newname",
		"ps e", "tput reset", "lsof +m", "test -v x", "man -P cat ls", "help ../x", "ifconfig eth0 up",
		"cd a b", "pwd -P", "history -c", "jq -f prog.js f", "jq --rawfile a b .", "jq env", "uniq a b c d e x",
		"find * -delete", "rm *.go", "echo $((1+2))", "ls $'x'", "git status; git push",
		"cat 'unterminated", "ls \"unterminated",
	}
	for _, c := range readOnly {
		if !IsReadOnlyBashCommand(c) {
			t.Errorf("IsReadOnlyBashCommand(%q) = false, want true", c)
		}
	}
	for _, c := range notReadOnly {
		if IsReadOnlyBashCommand(c) {
			t.Errorf("IsReadOnlyBashCommand(%q) = true, want false", c)
		}
	}
}

// Every read-only table entry is present, in the expected order.
func TestReadOnlyTableOrder(t *testing.T) {
	want := []string{"xargs", "git diff", "git log", "git show", "git shortlog", "git reflog", "git stash list", "git ls-remote", "git status", "git blame", "git ls-files", "git config --get", "git remote show", "git remote", "git merge-base", "git rev-parse", "git rev-list", "git describe", "git cat-file", "git for-each-ref", "git grep", "git stash show", "git worktree list", "git tag", "git branch", "file", "sed", "sort", "man", "help", "netstat", "ps", "base64", "grep", "egrep", "fgrep", "rg", "sha256sum", "sha1sum", "md5sum", "tree", "date", "hostname", "lsof", "pgrep", "tput", "ss", "fd", "fdfind", "docker logs", "docker inspect", "test"}
	if len(readOnlyCommandTable) != len(want) {
		t.Fatalf("table has %d entries, want %d", len(readOnlyCommandTable), len(want))
	}
	for i, w := range want {
		if readOnlyCommandTable[i].name != w {
			t.Errorf("entry %d = %q, want %q", i, readOnlyCommandTable[i].name, w)
		}
	}
	// Spot-check verbatim flag kinds.
	checks := map[string]map[string]string{
		"git log": {"--format": "string", "-n": "number", "--oneline": "none"},
		"sort":    {"-k": "string", "--parallel": "number"},
		"xargs":   {"-I": "{}", "-d": "char", "-E": "EOF"},
		"grep":    {"-A": "number", "-r": "none"},
		"rg":      {"-t": "string", "--": "none"},
	}
	for _, e := range readOnlyCommandTable {
		for f, k := range checks[e.name] {
			if e.safeFlags[f] != k {
				t.Errorf("%s %s = %q, want %q", e.name, f, e.safeFlags[f], k)
			}
		}
	}
	for _, e := range readOnlyCommandTable {
		if e.name == "sort" {
			if _, ok := e.safeFlags["-o"]; ok {
				t.Error("sort must not allow -o")
			}
		}
		if e.name == "rg" {
			if _, ok := e.safeFlags["--pre"]; ok {
				t.Error("rg must not allow --pre")
			}
		}
	}
}

// Read-only Bash is allowed by the tool's own check in default mode, but only
// for paths inside the working directory; rules still win.
func TestReadOnlyBashAutoAllowPaths(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	d := &Decider{Registry: DefaultRegistry(), PathRules: rootsAt(dir)}
	bash := func(cmd string) Call { return Call{Tool: protocol.ToolBash, Rule: "Bash", RuleContent: cmd} }
	for _, cmd := range []string{"ls", "cat a.txt", "grep -r x .", "git status", "ls " + dir, "head " + filepath.Join(dir, "a.txt")} {
		if dec := d.Decide(bash(cmd), ModeDefault, nil, nil); dec.Behavior != BehaviorAllow {
			t.Errorf("%q in default: got %s (%s), want allow", cmd, dec.Behavior, dec.Reason)
		}
	}
	for _, cmd := range []string{"cat " + filepath.Join(outside, "x"), "ls " + outside, "cat ../x", "cd .. && ls", "cd a && cd b", "rm a.txt"} {
		if dec := d.Decide(bash(cmd), ModeDefault, nil, nil); dec.Behavior != BehaviorAsk {
			t.Errorf("%q in default: got %s, want ask", cmd, dec.Behavior)
		}
	}
	// A Read allow rule covers an outside path.
	allowOutside := RuleSet{{Behavior: RuleAllow, ToolName: "Read", Content: "/" + filepath.ToSlash(outside) + "/**"}}
	if dec := d.Decide(bash("ls "+filepath.Join(outside, "x")), ModeDefault, allowOutside, nil); dec.Behavior != BehaviorAllow {
		t.Errorf("Read allow rule should cover outside path: got %s (%s)", dec.Behavior, dec.Reason)
	}
	// A Read deny rule on a read path denies.
	denyA := RuleSet{{Behavior: RuleDeny, ToolName: "Read", Content: "./a.txt"}}
	if dec := d.Decide(bash("cat a.txt"), ModeDefault, denyA, nil); dec.Behavior != BehaviorDeny {
		t.Errorf("Read deny rule: got %s, want deny", dec.Behavior)
	}
	// Bash ask / deny rules win over the read-only allow.
	if dec := d.Decide(bash("ls"), ModeDefault, RuleSet{{Behavior: RuleAsk, ToolName: "Bash", Content: "ls"}}, nil); dec.Behavior != BehaviorAsk {
		t.Errorf("ask rule: got %s", dec.Behavior)
	}
	if dec := d.Decide(bash("ls"), ModeDefault, RuleSet{{Behavior: RuleDeny, ToolName: "Bash"}}, nil); dec.Behavior != BehaviorDeny {
		t.Errorf("deny rule: got %s", dec.Behavior)
	}
	// dontAsk: the tool's own allow answers before the mode default.
	if dec := d.Decide(bash("ls"), ModeDontAsk, nil, nil); dec.Behavior != BehaviorAllow {
		t.Errorf("dontAsk read-only: got %s", dec.Behavior)
	}
	if dec := d.Decide(bash("rm x"), ModeDontAsk, nil, nil); dec.Behavior != BehaviorDeny {
		t.Errorf("dontAsk write: got %s", dec.Behavior)
	}
	// Plan mode: read-only passes the floor; writes are held.
	if dec := d.Decide(bash("git log -n 3"), ModePlan, nil, nil); dec.Behavior != BehaviorAllow {
		t.Errorf("plan read-only: got %s (%s)", dec.Behavior, dec.Reason)
	}
	if dec := d.Decide(bash("git commit -m x"), ModePlan, nil, nil); dec.Behavior != BehaviorAsk || !strings.Contains(dec.Reason, "plan mode") {
		t.Errorf("plan write: got %s %q", dec.Behavior, dec.Reason)
	}
}

func TestBashConcurrencySafeFollowsReadOnly(t *testing.T) {
	desc, ok := DefaultRegistry().Lookup(protocol.ToolBash)
	if !ok {
		t.Fatal("no Bash descriptor")
	}
	if !desc.IsConcurrencySafeFor("git status") || desc.IsConcurrencySafeFor("rm x") {
		t.Error("Bash concurrency safety should follow IsReadOnlyBashCommand")
	}
	if desc.ReadOnly || !desc.IsReadOnlyFor("ls") || desc.IsReadOnlyFor("touch x") {
		t.Error("Bash read-only-ness should be per command")
	}
}

func rootsAt(dir string) *pathguard.PathRuleRoots {
	home, _ := os.UserHomeDir()
	return &pathguard.PathRuleRoots{Cwd: dir, Home: home}
}

// TestGrepClassificationWithUgrepOptions: `grep` stays read-only for the
// usual flags while it runs ugrep in the shell, and the ugrep options that
// start programs or touch config files are never read-only.
func TestGrepClassificationWithUgrepOptions(t *testing.T) {
	for _, cmd := range []string{"grep -rn foo .", "grep -ril foo src", "grep -c foo a.txt", "grep -E 'a|b' f", "grep -A2 -B1 x f"} {
		if !IsReadOnlyBashCommand(cmd) {
			t.Errorf("%q must stay read-only", cmd)
		}
	}
	for _, cmd := range []string{
		"grep --pager=less foo f", "grep --view=vi foo f", "grep --filter='pdf:pdftotext % -' foo f",
		"grep --format-open='%f' foo f", "grep --config=x foo f", "grep --save-config foo f", "grep -Q foo",
	} {
		if IsReadOnlyBashCommand(cmd) {
			t.Errorf("%q must not be read-only", cmd)
		}
	}
}
