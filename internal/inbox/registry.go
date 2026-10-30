package inbox

// The local session registry: every gocode session writes a small record
// <configDir>/sessions/<pid>.json naming its inbox socket, so another session
// of the same user can discover it (ListAgents) and address it by name
// (SendMessage). The record is removed when the session exits.
//
// Not supported: procStart/pidDomain recycled-pid checks, the
// heartbeat file, the v5 storage backend and the per-socket auth-key files
// (peers are authenticated by OS user instead).

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// PeerProtocol is the peerProtocol value written to a record.
const PeerProtocol = 1

// SessionRecord is one <pid>.json record. Unknown fields written by other
// builds are ignored.
type SessionRecord struct {
	PID                 int    `json:"pid"`
	SessionID           string `json:"sessionId,omitempty"`
	Cwd                 string `json:"cwd,omitempty"`
	StartedAt           int64  `json:"startedAt,omitempty"`
	Version             string `json:"version,omitempty"`
	PeerProtocol        int    `json:"peerProtocol,omitempty"`
	Kind                string `json:"kind,omitempty"`
	Entrypoint          string `json:"entrypoint,omitempty"`
	MessagingSocketPath string `json:"messagingSocketPath,omitempty"`
	Name                string `json:"name,omitempty"`
	NameSource          string `json:"nameSource,omitempty"`
	NameSince           int64  `json:"nameSince,omitempty"`
	Status              string `json:"status,omitempty"`
	StatusUpdatedAt     int64  `json:"statusUpdatedAt,omitempty"`
	Spare               bool   `json:"spare,omitempty"`
	ParkedJobID         string `json:"parkedJobId,omitempty"`
}

// SessionsDir is <configDir>/sessions.
func SessionsDir(configDir string) string { return filepath.Join(configDir, "sessions") }

// recordNameRe is the record file shape.
var recordNameRe = regexp.MustCompile(`^\d+\.json$`)

// ErrRegistryUnreadable: the sessions directory exists but could not
// be read, so the local listing is incomplete.
var ErrRegistryUnreadable = errors.New("session records directory unreadable")

// RegisterSession writes rec as <dir>/<pid>.json (directory 0700, file 0600,
// written atomically) and returns the function that removes it again.
func RegisterSession(dir string, rec SessionRecord) (unregister func(), err error) {
	if rec.PID == 0 {
		rec.PID = os.Getpid()
	}
	if rec.StartedAt == 0 {
		rec.StartedAt = time.Now().UnixMilli()
	}
	if rec.PeerProtocol == 0 {
		rec.PeerProtocol = PeerProtocol
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	_ = os.Chmod(dir, 0o700)
	data, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, strconv.Itoa(rec.PID)+".json")
	tmp := path + ".tmp." + randomHex(4)
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	return func() { _ = os.Remove(path) }, nil
}

// readRecords returns every well-formed record in dir owned by this user. A
// missing directory is an empty registry; an unreadable one is
// ErrRegistryUnreadable.
func readRecords(dir string) ([]SessionRecord, []string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, ErrRegistryUnreadable
	}
	var recs []SessionRecord
	var files []string
	for _, e := range ents {
		if !recordNameRe.MatchString(e.Name()) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		fi, err := os.Lstat(path)
		if err != nil || !fi.Mode().IsRegular() || !recordOwnedByUs(fi) || fi.Size() > 64*1024 {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var r SessionRecord
		if json.Unmarshal(data, &r) != nil {
			continue
		}
		pid, _ := strconv.Atoi(strings.TrimSuffix(e.Name(), ".json"))
		if pid <= 0 {
			continue
		}
		r.PID = pid // the file name is canonical
		recs = append(recs, r)
		files = append(files, path)
	}
	return recs, files, nil
}

// ListLiveSessions returns the other live local sessions with an inbox. The
// record of ownSock (this session) is excluded, as are spare and parked
// records; a record whose process is gone is swept; a record whose socket
// does not answer is left out. Sorted oldest first for a stable listing.
func ListLiveSessions(dir, ownSock string) ([]SessionRecord, error) {
	recs, files, err := readRecords(dir)
	if err != nil {
		return nil, err
	}
	own := ""
	if ownSock != "" {
		own = canonicalEndpoint(ownSock)
	}
	var out []SessionRecord
	for i, r := range recs {
		if r.MessagingSocketPath == "" || r.Spare || r.ParkedJobID != "" {
			continue
		}
		if own != "" && canonicalEndpoint(r.MessagingSocketPath) == own {
			continue
		}
		if !pidAlive(r.PID) {
			_ = os.Remove(files[i]) // sweep a dead session's record
			continue
		}
		if !isLocalIPCPath(r.MessagingSocketPath) || !probeEndpoint(r.MessagingSocketPath) {
			continue
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].StartedAt < out[b].StartedAt })
	return out, nil
}

// DisplayName is the row name of a record: its name, else the basename of
// its cwd, cleaned.
func (r SessionRecord) DisplayName() string {
	n := r.Name
	if n == "" {
		n = filepath.Base(r.Cwd)
		if r.Cwd == "" || n == "." || n == string(filepath.Separator) {
			n = ""
		}
	}
	return cleanSessionName(n)
}

// sessionNameMax: a session name is capped at 200 code points.
const sessionNameMax = 200

// cleanSessionName: control/format characters dropped, whitespace
// collapsed, trimmed, capped at 200 code points; "" becomes
// "untitled session".
func cleanSessionName(s string) string {
	s = strings.Map(func(r rune) rune {
		if isControlOrFormat(r) && !isSpaceRune(r) {
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if rs := []rune(s); len(rs) > sessionNameMax {
		s = strings.TrimSpace(string(rs[:sessionNameMax]))
	}
	if s == "" {
		return "untitled session"
	}
	return s
}

// DerivedSessionName is "<basename of cwd>-<2 hex>" (or "gocode-<2 hex>"),
// the name an interactive session registers when the user chose none.
func DerivedSessionName(cwd string) string {
	base := ""
	if cwd != "" {
		base = filepath.Base(cwd)
	}
	var b strings.Builder
	for _, r := range strings.ToLower(base) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		case isSpaceRune(r):
			b.WriteRune('-')
		}
	}
	name := strings.Trim(b.String(), "-.")
	if name == "" {
		name = "gocode"
	}
	return name + "-" + randomHex(1)
}

// isControlOrFormat is the [\p{Cc}\p{Cf}\p{Cs}] class cleanSessionName strips.
func isControlOrFormat(r rune) bool {
	return unicode.In(r, unicode.Cc, unicode.Cf, unicode.Cs)
}

// isSpaceRune matches /\s/.
func isSpaceRune(r rune) bool { return unicode.IsSpace(r) || r == '\ufeff' }
