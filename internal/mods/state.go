package mods

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/dop251/goja/ast"
)

// stateStore is $.state: named values the host holds for the session, each
// with a version (a store of { value, version }, and its
// versionFloor). A value never written is undefined at version 0; a write
// takes the next version past both the value's and the floor.
//
// They are kept in memory for the session (they survive a hot
// reload of the plugin's code; "Persist through $.store"): nothing of
// $.state reaches the disk.
type stateStore struct {
	mu     sync.Mutex
	values map[string]stateValue
	floor  float64
}

type stateValue struct {
	value   any
	version float64
}

func newStateStore() *stateStore { return &stateStore{values: map[string]stateValue{}} }

// stateRef is a { plugin, key, id? } reference.
type stateRef struct{ plugin, key, id string }

func refOf(a map[string]any) stateRef {
	r := stateRef{}
	r.plugin, _ = a["plugin"].(string)
	r.key, _ = a["key"].(string)
	r.id, _ = a["id"].(string)
	return r
}

// name: plugin NUL key [NUL id].
func (r stateRef) name() string {
	if r.id == "" {
		return r.plugin + "\x00" + r.key
	}
	return r.plugin + "\x00" + r.key + "\x00" + r.id
}

func (s *stateStore) get(r stateRef) (any, float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.values[r.name()]
	if !ok {
		return nil, 0
	}
	return v.value, v.version
}

// set: a write unless ifVersion names another version than the one
// the value stands at.
func (s *stateStore) set(r stateRef, value any, ifVersion *float64) (bool, float64, error) {
	b, err := json.Marshal(value)
	if err != nil || len(b) > storeValueLimit {
		return false, 0, fmt.Errorf("a value of state is JSON data of at most %d characters", storeValueLimit)
	}
	var copied any
	_ = json.Unmarshal(b, &copied)
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.values[r.name()].version
	if ifVersion != nil && *ifVersion != cur {
		return false, cur, nil
	}
	next := max(cur, s.floor) + 1
	s.values[r.name()] = stateValue{value: copied, version: next}
	return true, next, nil
}

// previous: the value a state.set replaces, stamped on its event.
func (s *stateStore) previous(r stateRef) any {
	v, _ := s.get(r)
	return v
}

const stateRefShape = "takes a reference { plugin, key } (id for a family member)"

func stateRefCheck(a map[string]any) string {
	p, pok := a["plugin"].(string)
	k, kok := a["key"].(string)
	id, has := a["id"]
	ids, idok := id.(string)
	if !pok || p == "" || !kok || k == "" || (has && id != nil && !idok) {
		return stateRefShape
	}
	for _, x := range []string{p, k, ids} {
		if strings.Contains(x, "\x00") {
			return "a plugin, key or id holding the NUL character"
		}
	}
	return ""
}

func stateGetCheck(a map[string]any) string {
	if m := stateRefCheck(a); m != "" {
		return m
	}
	for _, k := range sortedKeysAny(a) {
		if k != "plugin" && k != "key" && k != "id" {
			return "takes { plugin, key, id? } and nothing else (got " + k + ")"
		}
	}
	return ""
}

var stateSetKeys = []string{"plugin", "key", "id", "value", "previous", "ifVersion"}

// stateSetCheck checks a state.set's arguments, less its owner rule
// (stateOwnerCheck).
func stateSetCheck(a map[string]any) string {
	if m := stateRefCheck(a); m != "" {
		return m
	}
	for _, k := range sortedKeysAny(a) {
		if !slices.Contains(stateSetKeys, k) {
			return "takes { plugin, key, id?, value, ifVersion? } and nothing else (got " + k + ")"
		}
	}
	v, has := a["value"]
	if !has {
		return "takes a value (JSON data, never undefined)"
	}
	if _, undef := v.(jsUndefined); undef {
		return "takes a value (JSON data, never undefined)"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "value is not JSON data: " + err.Error()
	}
	if len(b) > storeValueLimit {
		return fmt.Sprintf("the value is %d characters, over the %d limit", len(b), storeValueLimit)
	}
	if iv, has := a["ifVersion"]; has && iv != nil {
		f, ok := iv.(float64)
		if !ok || f < 0 || f != float64(int64(f)) || f > 9007199254740991 {
			return "ifVersion is a version $.state.get answered (a whole number)"
		}
	}
	return ""
}

// stateOwnerCheck is the caller's half of the checks: only a value's owner
// writes it, and a plugin reads only its own values: each plugin's state is
// kept to itself.
func stateOwnerCheck(name string, a map[string]any, caller string) string {
	owner, _ := a["plugin"].(string)
	if owner == caller {
		return ""
	}
	if name == "state.set" {
		return fmt.Sprintf("denied: %s owns that value and only its owner writes it (to change what is written, hook %s's state.set and rewrite e.value)", owner, owner)
	}
	return fmt.Sprintf("denied: %s owns that value; this gocode build keeps a plugin's state to itself, so another plugin does not read it", owner)
}

// StateRef is a { plugin, key } a module's source names in a $.state call.
type StateRef struct {
	Plugin string `json:"plugin"`
	Key    string `json:"key"`
}

// StateScan lists the state values a module reads and writes.
type StateScan struct {
	Reads  []StateRef `json:"reads"`
	Writes []StateRef `json:"writes"`
}

// allowsState: state.get / state.set serve only the references
// the scan lists.
func (s *Scan) allowsState(op string, a map[string]any) string {
	if op != "state.get" && op != "state.set" {
		return ""
	}
	p, pok := a["plugin"].(string)
	k, kok := a["key"].(string)
	if !pok || !kok {
		return fmt.Sprintf("it carries no reference a $.%s({ plugin, key }) call passes", op)
	}
	var list []StateRef
	kind := "writes"
	if s != nil && s.State != nil {
		if op == "state.get" {
			list = s.State.Reads
		} else {
			list = s.State.Writes
		}
	}
	if op == "state.get" {
		kind = "reads"
	}
	want := StateRef{Plugin: p, Key: k}
	if slices.Contains(list, want) {
		return ""
	}
	listed := "nothing"
	if len(list) > 0 {
		parts := make([]string, len(list))
		for i, r := range list {
			parts[i] = stateRefText(r)
		}
		listed = strings.Join(parts, ", ")
	}
	return fmt.Sprintf("its hooks module does not refer to %s in a $.%s({ plugin, key }) call (host rule; the scan lists %s of %s)", stateRefText(want), op, kind, listed)
}

// stateRefText renders a state reference as text.
func stateRefText(r StateRef) string {
	q := func(s string) string {
		for _, c := range s {
			if !(c == '_' || c == '$' || c == '@' || c == '-' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
				b, _ := json.Marshal(s)
				return string(b)
			}
		}
		if s == "" {
			return `""`
		}
		return s
	}
	return q(r.Plugin) + "." + q(r.Key)
}

// stateScanner collects the { plugin, key } references of $.state calls: a
// reference written at the call, or a const of the file bound to one ("a
// reference whose plugin and key are string literals ... written there or
// in a const of this file").
type stateScanner struct {
	consts map[string][]StateRef
	reads  map[StateRef]bool
	writes map[StateRef]bool
	bad    []string
}

func newStateScanner(prog *ast.Program) *stateScanner {
	sc := &stateScanner{consts: map[string][]StateRef{}, reads: map[StateRef]bool{}, writes: map[StateRef]bool{}}
	walk(prog, func(n ast.Node) {
		b, ok := n.(*ast.Binding)
		if !ok {
			return
		}
		id, ok := b.Target.(*ast.Identifier)
		if !ok || b.Initializer == nil {
			return
		}
		if r, ok := literalStateRef(b.Initializer); ok {
			sc.consts[string(id.Name)] = append(sc.consts[string(id.Name)], r)
		}
	})
	return sc
}

// literalStateRef reads { plugin: "<lit>", key: "<lit>", ... }.
func literalStateRef(e ast.Expression) (StateRef, bool) {
	obj, ok := e.(*ast.ObjectLiteral)
	if !ok {
		return StateRef{}, false
	}
	var r StateRef
	for _, p := range obj.Value {
		pk, ok := p.(*ast.PropertyKeyed)
		if !ok {
			continue
		}
		var name string
		switch k := pk.Key.(type) {
		case *ast.StringLiteral:
			name = string(k.Value)
		case *ast.Identifier:
			name = string(k.Name)
		default:
			continue
		}
		lit, isLit := pk.Value.(*ast.StringLiteral)
		switch name {
		case "plugin":
			if !isLit {
				return StateRef{}, false
			}
			r.Plugin = string(lit.Value)
		case "key":
			if !isLit {
				return StateRef{}, false
			}
			r.Key = string(lit.Value)
		}
	}
	return r, r.Plugin != "" && r.Key != ""
}

// call notes one $.state.get / $.state.set call.
func (sc *stateScanner) call(op string, args []ast.Expression) {
	if len(args) == 0 {
		sc.bad = append(sc.bad, op)
		return
	}
	var refs []StateRef
	switch a := args[0].(type) {
	case *ast.ObjectLiteral:
		if r, ok := literalStateRef(a); ok {
			refs = []StateRef{r}
		}
	case *ast.Identifier:
		refs = sc.consts[string(a.Name)]
	}
	if len(refs) == 0 {
		sc.bad = append(sc.bad, op)
		return
	}
	for _, r := range refs {
		if op == "state.set" {
			sc.writes[r] = true
		} else {
			sc.reads[r] = true
		}
	}
}

func (sc *stateScanner) result() *StateScan {
	if len(sc.reads) == 0 && len(sc.writes) == 0 {
		return nil
	}
	list := func(m map[StateRef]bool) []StateRef {
		out := make([]StateRef, 0, len(m))
		for r := range m {
			out = append(out, r)
		}
		sort.Slice(out, func(i, j int) bool {
			return out[i].Plugin+"\x00"+out[i].Key < out[j].Plugin+"\x00"+out[j].Key
		})
		return out
	}
	return &StateScan{Reads: list(sc.reads), Writes: list(sc.writes)}
}
