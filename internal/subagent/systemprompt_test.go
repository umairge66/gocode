package subagent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/protocol"
)

// TestAssembleSystemPrompt checks the assembly order (persona, tool
// guidance, memory) and that the hand-back contract ALWAYS terminates the
// prompt without being duplicated.
func TestAssembleSystemPrompt(t *testing.T) {
	got := AssembleSystemPrompt("You are the X subagent.", []string{"Read", "Bash", string(protocol.CmdSubagentHandback)}, "Remembered: use tabs.")

	if !strings.HasPrefix(got, "You are the X subagent.") {
		t.Fatalf("persona is not the prompt prefix:\n%s", got)
	}
	if !strings.HasSuffix(got, HandbackContract) {
		t.Fatalf("prompt does not end with the hand-back contract:\n%s", got)
	}
	if strings.Count(got, HandbackContract) != 1 {
		t.Fatalf("hand-back contract appears %d times, want once", strings.Count(got, HandbackContract))
	}
	if !strings.Contains(got, "## Tools") || !strings.Contains(got, "Read, Bash") {
		t.Fatalf("tool guidance missing or wrong:\n%s", got)
	}
	// SubagentHandback is described by the contract, not listed as a task tool.
	if strings.Contains(got, "Read, Bash, SubagentHandback") {
		t.Fatalf("hand-back tool should not be listed in the tool guidance:\n%s", got)
	}
	if !strings.Contains(got, "Remembered: use tabs.") {
		t.Fatalf("memory section missing:\n%s", got)
	}
	// Ordering: persona < tools < memory < contract.
	iTools := strings.Index(got, "## Tools")
	iMem := strings.Index(got, "Remembered: use tabs.")
	iHB := strings.Index(got, HandbackContract)
	if !(iTools < iMem && iMem < iHB) {
		t.Fatalf("section order wrong (tools=%d mem=%d contract=%d):\n%s", iTools, iMem, iHB, got)
	}
}

// TestAssembleSystemPromptOmitsEmptySections: with no tools and no memory the
// prompt is persona + contract only, and still ends with the contract.
func TestAssembleSystemPromptOmitsEmptySections(t *testing.T) {
	got := AssembleSystemPrompt("Persona.", nil, "")
	if got != "Persona.\n\n"+HandbackContract {
		t.Fatalf("unexpected minimal prompt:\n%q", got)
	}
	// A tool list of only the hand-back tool yields no tool guidance.
	got = AssembleSystemPrompt("Persona.", []string{string(protocol.CmdSubagentHandback)}, "")
	if strings.Contains(got, "## Tools") {
		t.Fatalf("tool guidance should be omitted when only the hand-back tool is present:\n%s", got)
	}
}

// TestAssembleSystemPromptWildcardTools: an inherited-all ("*") tool set renders
// as "the full tool set" rather than a literal wildcard.
func TestAssembleSystemPromptWildcardTools(t *testing.T) {
	got := AssembleSystemPrompt("P.", []string{"*"}, "")
	if !strings.Contains(got, "full tool set") || strings.Contains(got, "these tools: *") {
		t.Fatalf("wildcard tool guidance wrong:\n%s", got)
	}
}

func TestDefaultPersona(t *testing.T) {
	p := DefaultPersona("Explore")
	if !strings.Contains(p, "Explore subagent") || !strings.Contains(p, "report") {
		t.Fatalf("default persona = %q", p)
	}
	// Empty type falls back to the general-purpose name.
	if !strings.Contains(DefaultPersona(""), DefaultAgentType) {
		t.Fatalf("empty-type persona lacks default name: %q", DefaultPersona(""))
	}
}

// TestSpawnerSystemPromptDefaultPath: with no SystemPromptFor hook, the prompt
// is assembled from the default persona, the child's tool schemas and
// the injected MemoryFor seam; a nil seam omits memory without crashing.
func TestSpawnerSystemPromptDefaultPath(t *testing.T) {
	a, _ := NewAgentTypeRegistry().Resolve("general-purpose")

	sp := &Spawner{
		Registry: NewTaskRegistry(),
		Types:    NewAgentTypeRegistry(),
		ToolSchemasFor: func(AgentType, SpawnRequest) []agent.ToolSchema {
			return []agent.ToolSchema{{Name: "Read"}, {Name: "Grep"}}
		},
		MemoryFor: func(AgentType, SpawnRequest) string { return "Primed: project uses Go 1.26." },
	}
	got := sp.systemPrompt(a, SpawnRequest{})
	if !strings.HasPrefix(got, "You are the general-purpose subagent") {
		t.Fatalf("persona default not used:\n%s", got)
	}
	if !strings.Contains(got, "Read, Grep") {
		t.Fatalf("tool guidance does not reflect allowed tools:\n%s", got)
	}
	if !strings.Contains(got, "Primed: project uses Go 1.26.") {
		t.Fatalf("injected memory not present:\n%s", got)
	}
	if !strings.HasSuffix(got, HandbackContract) {
		t.Fatalf("prompt must end with the hand-back contract:\n%s", got)
	}

	// Nil MemoryFor: no memory, no crash, still terminated by the contract.
	sp.MemoryFor = nil
	got = sp.systemPrompt(a, SpawnRequest{})
	if strings.Contains(got, "Primed:") {
		t.Fatalf("memory should be absent when MemoryFor is nil:\n%s", got)
	}
	if !strings.HasSuffix(got, HandbackContract) {
		t.Fatalf("nil-memory prompt must still end with the contract:\n%s", got)
	}
}

// TestSpawnerSystemPromptHookEnsuresContract: a SystemPromptFor hook result is
// used as the base, and the hand-back contract is guaranteed to terminate it —
// appended when missing, not duplicated when already present.
func TestSpawnerSystemPromptHookEnsuresContract(t *testing.T) {
	a, _ := NewAgentTypeRegistry().Resolve("general-purpose")

	sp := &Spawner{Registry: NewTaskRegistry(), Types: NewAgentTypeRegistry()}
	sp.SystemPromptFor = func(AgentType, SpawnRequest) string { return "Bare persona with no contract." }
	got := sp.systemPrompt(a, SpawnRequest{})
	if !strings.HasPrefix(got, "Bare persona with no contract.") || !strings.HasSuffix(got, HandbackContract) {
		t.Fatalf("hook base not terminated by contract:\n%s", got)
	}

	// Hook already appends the contract (e.g. via AssembleSystemPrompt): no dup.
	sp.SystemPromptFor = func(at AgentType, req SpawnRequest) string {
		return AssembleSystemPrompt("Assembled persona.", []string{"Read"}, "")
	}
	got = sp.systemPrompt(a, SpawnRequest{})
	if strings.Count(got, HandbackContract) != 1 {
		t.Fatalf("contract duplicated:\n%s", got)
	}
}

// recordingStopModel records each turn's system prompt. Turn 1 blocks until the
// context is cancelled (so a Stop can interrupt it); turn 2 hands back; later
// turns end.
type recordingStopModel struct {
	mu      sync.Mutex
	systems []string
	calls   int
	entered chan struct{}
	once    sync.Once
}

func (m *recordingStopModel) CallModel(ctx context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	m.mu.Lock()
	m.calls++
	n := m.calls
	m.systems = append(m.systems, req.System)
	m.mu.Unlock()
	switch n {
	case 1:
		m.once.Do(func() { close(m.entered) })
		<-ctx.Done()
		return agent.ModelResponse{}, ctx.Err()
	case 2:
		return handbackResp("hb", "resumed report"), nil
	default:
		return agent.ModelResponse{Message: agent.AssistantMessage(agent.TextBlock("done")), StopReason: agent.StopEndTurn}, nil
	}
}

// TestSpawnResumeSameSystemPrompt: the initial spawn turn and a later resumed
// turn run with the identical system prompt (stored once on the session), and
// that prompt is a fully assembled one (persona + hand-back contract).
func TestSpawnResumeSameSystemPrompt(t *testing.T) {
	sp := &Spawner{
		Registry: NewTaskRegistry(),
		Types:    NewAgentTypeRegistry(),
		ToolSchemasFor: func(AgentType, SpawnRequest) []agent.ToolSchema {
			return []agent.ToolSchema{{Name: "Read"}}
		},
	}
	m := &recordingStopModel{entered: make(chan struct{})}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = sp.Spawn(context.Background(), SpawnRequest{Prompt: "do work"}, m, nopDispatcher{})
	}()

	<-m.entered
	mgr := sp.SessionManager()
	ids := mgr.ActiveIDs()
	if len(ids) != 1 {
		t.Fatalf("active sessions = %v, want one", ids)
	}
	id := ids[0]
	if err := mgr.Stop(id); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	<-done // first turn settled as Stopped

	if err := mgr.Resume(context.Background(), id, "finish up"); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	// Turn 1 is the spawn; the resume contributes the remaining calls (hand-back
	// then the loop's follow-up round). Every turn must use the same stored
	// system prompt.
	if len(m.systems) < 2 {
		t.Fatalf("recorded %d system prompts, want at least 2 (spawn + resume)", len(m.systems))
	}
	for i, s := range m.systems {
		if s != m.systems[0] {
			t.Fatalf("turn %d used a different system prompt than the spawn:\n--- spawn ---\n%s\n--- turn %d ---\n%s", i, m.systems[0], i, s)
		}
	}
	if !strings.HasPrefix(m.systems[0], "You are the general-purpose subagent") {
		t.Fatalf("spawn prompt missing persona:\n%s", m.systems[0])
	}
	if !strings.HasSuffix(m.systems[0], HandbackContract) {
		t.Fatalf("spawn/resume prompt not terminated by the hand-back contract:\n%s", m.systems[0])
	}
}
