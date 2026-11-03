package sessionstore

import (
	"bufio"
	"bytes"
	"encoding/json"
	"time"

	"github.com/umairge66/gocode/internal/cost"
)

// Cost state lines.
//
// After a turn that spent anything, the conversation appends the session's
// cumulative cost totals as a meta line:
//
//	{"type":"cost-state","timestamp":…,"sessionId":…,"gocodeSchema":1,"costState":{…}}
//
// It carries no uuid/message, so it is not part of the turn chain and every
// reader of the history skips it (older builds included). On resume the last
// such line restores the ledger (LoadCostState); a transcript without one
// resumes with an empty ledger, as before.

const typeCostState = "cost-state"

// costStateLine is the on-disk shape of a cost-state line.
type costStateLine struct {
	Type         string      `json:"type"`
	Timestamp    string      `json:"timestamp,omitempty"`
	SessionID    string      `json:"sessionId,omitempty"`
	GocodeSchema int         `json:"gocodeSchema,omitempty"`
	CostState    *cost.State `json:"costState"`
}

// AppendCostState appends the session's cumulative cost totals. Implements
// conversation.CostStateRecorder.
func (s *Session) AppendCostState(st cost.State) error {
	if s == nil {
		return nil
	}
	if err := s.ensureDir(); err != nil {
		return err
	}
	data, err := json.Marshal(costStateLine{
		Type:         typeCostState,
		Timestamp:    time.Now().UTC().Format(time.RFC3339Nano),
		SessionID:    s.id,
		GocodeSchema: SchemaVersion,
		CostState:    &st,
	})
	if err != nil {
		return err
	}
	w, err := s.store.fs.OpenAppend(s.path, filePerm)
	if err != nil {
		return err
	}
	defer w.Close()
	_, err = w.Write(append(data, '\n'))
	return err
}

// LoadCostState returns the last cost totals recorded in session id's
// transcript; ok is false when there are none (or the file cannot be read).
func (s *Store) LoadCostState(cwd, id string) (cost.State, bool) {
	if s == nil || s.base == "" {
		return cost.State{}, false
	}
	data, err := s.fs.ReadFile(s.sessionPath(cwd, id))
	if err != nil {
		return cost.State{}, false
	}
	var (
		last cost.State
		ok   bool
	)
	marker := []byte(`"type":"` + typeCostState + `"`)
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, marker) {
			continue
		}
		var l costStateLine
		if json.Unmarshal(line, &l) != nil || l.Type != typeCostState || l.CostState == nil {
			continue
		}
		last, ok = *l.CostState, true
	}
	return last, ok
}
