package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseMaxBudgetFlag(t *testing.T) {
	for v, want := range map[string]float64{"5": 5, "0.25": 0.25, " $1.5 ": 1.5} {
		if got, err := parseMaxBudgetFlag(v); err != nil || got != want {
			t.Errorf("%q → %v, %v; want %v", v, got, err, want)
		}
	}
	for _, v := range []string{"", "0", "-1", "abc", "NaN", "Inf"} {
		if _, err := parseMaxBudgetFlag(v); err == nil || !strings.Contains(err.Error(), "--max-budget-usd must be a positive number") {
			t.Errorf("%q accepted (%v)", v, err)
		}
	}
	d, rest := parseDispatchDefaults([]string{"-p", "--max-budget-usd", "3", "hi"})
	if d == nil || !d.MaxBudgetUSDSet || d.MaxBudgetUSD != "3" || strings.Join(rest, " ") != "-p hi" {
		t.Errorf("parsed %+v, rest %v", d, rest)
	}
}

// --max-budget-usd joins the maxSessionCostUsd / GOCODE_MAX_SESSION_COST_USD
// precedence: after a policy value, ahead of the environment and settings.
func TestResolveMaxSessionCostFlag(t *testing.T) {
	env, cwd, _ := capEnv(t, map[string]string{maxSessionCostEnv: "7"})
	writeJSON(t, filepath.Join(cwd, ".gocode", "settings.local.json"), `{"maxSessionCostUsd": 2.5}`)
	if got := resolveMaxSessionCost(env, 3); got != 3 {
		t.Errorf("flag over env: %v", got)
	}
	if got := resolveMaxSessionCost(env, 0); got != 7 {
		t.Errorf("no flag: %v", got)
	}
	env, _, managed := capEnv(t, nil)
	writeJSON(t, filepath.Join(managed, "managed-settings.json"), `{"maxSessionCostUsd": 1}`)
	if got := resolveMaxSessionCost(env, 3); got != 1 {
		t.Errorf("policy over flag: %v", got)
	}
	if got := sessionMaxBudget(env, &printSession{MaxBudgetUSD: 9}); got != 1 {
		t.Errorf("sessionMaxBudget: %v", got)
	}
}

// In -p the turn stops after the response that crossed the cap: the result
// is error_max_budget_usd with its terminal reason, and the model is not
// called again.
func TestPrintJSONMaxBudget(t *testing.T) {
	p := newFakeProvider(t, fakeTurn{toolName: "Glob", toolArgs: `{"pattern":"*.none"}`, in: 100000, out: 1000})
	f := headlessFixture(t, p)
	code, out, errOut := f.run(t, "-p", "--output-format", "json", "--max-budget-usd", "0.0001", "loop forever")
	if code != 1 {
		t.Fatalf("exit %d, err %q out %q", code, errOut, out)
	}
	var r map[string]any
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("bad json %q", out)
	}
	if r["subtype"] != "error_max_budget_usd" || r["is_error"] != true || r["terminal_reason"] != "max_budget_usd" {
		t.Fatalf("result = %v", r)
	}
	errs, _ := r["errors"].([]any)
	if len(errs) != 1 || !strings.Contains(errs[0].(string), "Spend cap reached") || !strings.Contains(errs[0].(string), "$0.0001 limit") {
		t.Fatalf("errors = %v", errs)
	}
	if cost, _ := r["total_cost_usd"].(float64); cost < 0.0001 {
		t.Errorf("total_cost_usd = %v, under the cap", cost)
	}
	if p.callCount() != 1 {
		t.Fatalf("model called %d times", p.callCount())
	}
}

func TestPrintMaxBudgetFlagValidated(t *testing.T) {
	p := newFakeProvider(t, fakeTurn{text: "hi", in: 1, out: 1})
	f := headlessFixture(t, p)
	code, _, errOut := f.run(t, "-p", "--max-budget-usd", "lots", "hi")
	if code != 1 || !strings.Contains(errOut, "--max-budget-usd must be a positive number") {
		t.Fatalf("exit %d, err %q", code, errOut)
	}
	if p.callCount() != 0 {
		t.Fatalf("model called %d times", p.callCount())
	}
}
