package interact

import (
	"encoding/json"
	"testing"

	"github.com/umairge66/gocode/internal/protocol"
)

func f64(v float64) *float64 { return &v }

func TestAskName(t *testing.T) {
	if got := (AskUserQuestionTool{}).Name(); got != protocol.ToolAskUserQuestion {
		t.Fatalf("Name() = %q", got)
	}
}

func TestAskInputUnmarshal(t *testing.T) {
	raw := `{"questions":[{"question":"Which library?","header":"Library","options":[{"label":"A","description":"first"},{"label":"B","description":"second"}],"multiSelect":false}]}`
	var in AskUserQuestionInput
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatal(err)
	}
	if len(in.Questions) != 1 || len(in.Questions[0].Options) != 2 {
		t.Fatalf("unexpected parse: %+v", in)
	}
	if in.Questions[0].Header != "Library" {
		t.Fatalf("header = %q", in.Questions[0].Header)
	}
}

func q(question, header string, labels ...string) Question {
	opts := make([]QuestionOption, len(labels))
	for i, l := range labels {
		opts[i] = QuestionOption{Label: l, Description: "d"}
	}
	return Question{Question: question, Header: header, Options: opts}
}

func TestAskValidation(t *testing.T) {
	tool := AskUserQuestionTool{}

	// valid
	if err := tool.Validate(AskUserQuestionInput{Questions: []Question{q("Pick?", "Head", "A", "B")}}); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}

	// zero questions
	if err := tool.Validate(AskUserQuestionInput{}); err == nil {
		t.Fatal("expected error for 0 questions")
	}

	// too many questions
	many := []Question{
		q("a?", "h", "A", "B"), q("b?", "h", "A", "B"),
		q("c?", "h", "A", "B"), q("d?", "h", "A", "B"), q("e?", "h", "A", "B"),
	}
	if err := tool.Validate(AskUserQuestionInput{Questions: many}); err == nil {
		t.Fatal("expected error for >4 questions")
	}

	// too few options
	if err := tool.Validate(AskUserQuestionInput{Questions: []Question{q("x?", "h", "only")}}); err == nil {
		t.Fatal("expected error for <2 options")
	}

	// duplicate question text
	dup := []Question{q("same?", "h", "A", "B"), q("same?", "h", "C", "D")}
	if err := tool.Validate(AskUserQuestionInput{Questions: dup}); err == nil {
		t.Fatal("expected error for duplicate question texts")
	}

	// duplicate option labels
	dupOpt := Question{Question: "x?", Header: "h", Options: []QuestionOption{{Label: "A"}, {Label: "A"}}}
	if err := tool.Validate(AskUserQuestionInput{Questions: []Question{dupOpt}}); err == nil {
		t.Fatal("expected error for duplicate option labels")
	}

	// header too long
	long := q("x?", "ThisHeaderIsWayTooLong", "A", "B")
	if err := tool.Validate(AskUserQuestionInput{Questions: []Question{long}}); err == nil {
		t.Fatal("expected error for long header")
	}
}

func TestAskValidationTextKind(t *testing.T) {
	tool := AskUserQuestionTool{}
	// text question with no options is valid
	tq := Question{Question: "Name?", Header: "Name", Kind: QuestionKindText}
	if err := tool.Validate(AskUserQuestionInput{Questions: []Question{tq}}); err != nil {
		t.Fatalf("text question rejected: %v", err)
	}
	// text question with options is invalid
	tq2 := Question{Question: "Name?", Header: "Name", Kind: QuestionKindText, Options: []QuestionOption{{Label: "A"}, {Label: "B"}}}
	if err := tool.Validate(AskUserQuestionInput{Questions: []Question{tq2}}); err == nil {
		t.Fatal("expected error: text question with options")
	}
}

func TestAskValidationNumberKind(t *testing.T) {
	tool := AskUserQuestionTool{}
	// valid number
	nq := Question{Question: "How many?", Header: "Count", Kind: QuestionKindNumber, Min: f64(1), Max: f64(10), DefaultValue: f64(5)}
	if err := tool.Validate(AskUserQuestionInput{Questions: []Question{nq}}); err != nil {
		t.Fatalf("number question rejected: %v", err)
	}
	// min >= max
	bad := Question{Question: "n?", Header: "C", Kind: QuestionKindNumber, Min: f64(10), Max: f64(1)}
	if err := tool.Validate(AskUserQuestionInput{Questions: []Question{bad}}); err == nil {
		t.Fatal("expected error: min >= max")
	}
	// defaultValue out of range
	oor := Question{Question: "n?", Header: "C", Kind: QuestionKindNumber, Min: f64(1), Max: f64(10), DefaultValue: f64(99)}
	if err := tool.Validate(AskUserQuestionInput{Questions: []Question{oor}}); err == nil {
		t.Fatal("expected error: defaultValue out of range")
	}
}

func TestAskExecuteBuildsRequest(t *testing.T) {
	tool := AskUserQuestionTool{}
	in := AskUserQuestionInput{Questions: []Question{q("Which?", "Pick", "A", "B"), {Question: "Free?", Header: "Note", Kind: QuestionKindText}}}
	req, err := tool.Execute(in)
	if err != nil {
		t.Fatal(err)
	}
	if req.Tool != protocol.ToolAskUserQuestion {
		t.Fatalf("req.Tool = %q", req.Tool)
	}
	if len(req.Questions) != 2 {
		t.Fatalf("expected 2 questions, got %d", len(req.Questions))
	}
	if req.Questions[0].Options == nil {
		t.Fatal("options should be non-nil slice")
	}
	// text question gets empty (non-nil) options slice
	if req.Questions[1].Options == nil || len(req.Questions[1].Options) != 0 {
		t.Fatalf("text question options = %+v", req.Questions[1].Options)
	}
}

func TestAskExecuteRejectsInvalid(t *testing.T) {
	if _, err := (AskUserQuestionTool{}).Execute(AskUserQuestionInput{}); err == nil {
		t.Fatal("expected error for empty questions")
	}
}

func TestStringOrListJSON(t *testing.T) {
	// unmarshal single string
	var s StringOrList
	if err := json.Unmarshal([]byte(`"A"`), &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Values) != 1 || s.Values[0] != "A" {
		t.Fatalf("single: %+v", s)
	}
	// unmarshal array
	var l StringOrList
	if err := json.Unmarshal([]byte(`["A","B"]`), &l); err != nil {
		t.Fatal(err)
	}
	if len(l.Values) != 2 {
		t.Fatalf("array: %+v", l)
	}
	// marshal joins with ", "
	out, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `"A, B"` {
		t.Fatalf("marshal = %s", out)
	}
}

func TestAskAnswersRoundTrip(t *testing.T) {
	raw := `{"questions":[{"question":"Which?","header":"Pick","options":[{"label":"A","description":"d"},{"label":"B","description":"d"}]}],"answers":{"Which?":["A","B"]}}`
	var in AskUserQuestionInput
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatal(err)
	}
	ans, ok := in.Answers["Which?"]
	if !ok || len(ans.Values) != 2 {
		t.Fatalf("answers = %+v", in.Answers)
	}
}
