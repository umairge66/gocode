package interact

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/umairge66/gocode/internal/protocol"
)

// AskUserQuestion schema bounds.
const (
	// AskMinQuestions is the minimum number of questions per call.
	AskMinQuestions = 1
	// AskMaxQuestions is the maximum number of questions per call.
	AskMaxQuestions = 4
	// AskMinOptions is the minimum options for a choice question.
	AskMinOptions = 2
	// AskMaxOptions is the maximum options for any question.
	AskMaxOptions = 4
	// AskHeaderMaxChars is the max length of a question header chip.
	AskHeaderMaxChars = 12
)

// Question "kind" enum from the extended schema.
const (
	QuestionKindChoice = "choice"
	QuestionKindText   = "text"
	QuestionKindNumber = "number"
)

// AskUserQuestionDescription is the short description text.
const AskUserQuestionDescription = "Asks the user multiple choice questions to gather information, clarify ambiguity, understand preferences, make decisions or offer them choices."

// AskNotesOnly marks a per-question answer that carried only notes (no choice).
const AskNotesOnly = "(notes only)"

// QuestionOption is a single selectable choice.
type QuestionOption struct {
	// Label is the concise (1-5 word) display text the user selects.
	Label string `json:"label"`
	// Description explains what the option means / its implications. In the
	// extended option schema this is optional.
	Description string `json:"description,omitempty"`
	// Preview is optional rendered content (mockups, code snippets) shown when
	// the option is focused; single-select questions only.
	Preview string `json:"preview,omitempty"`
}

// Question is one question to put to the user. The base fields (question,
// header, options, multiSelect) come from the base schema; the remaining
// optional fields come from the extended schema (enabled at runtime).
type Question struct {
	// Question is the full question text (should end with a question mark).
	Question string `json:"question"`
	// Header is the very short chip/tag label (max AskHeaderMaxChars chars).
	Header string `json:"header"`
	// Options are the available choices (2-4 for a choice question).
	Options []QuestionOption `json:"options,omitempty"`
	// MultiSelect allows selecting multiple options (choice questions only).
	MultiSelect bool `json:"multiSelect,omitempty"`

	// --- extended schema: optional ---

	// Kind is how the user answers: "choice" (default), "text" or "number".
	Kind string `json:"kind,omitempty"`
	// Description is an optional single helper line under the question.
	Description string `json:"description,omitempty"`
	// Placeholder is the empty-box placeholder ("text" questions only).
	Placeholder string `json:"placeholder,omitempty"`
	// Min / Max / Step / DefaultValue / Unit apply to "number" questions.
	Min          *float64 `json:"min,omitempty"`
	Max          *float64 `json:"max,omitempty"`
	Step         *float64 `json:"step,omitempty"`
	DefaultValue *float64 `json:"defaultValue,omitempty"`
	Unit         string   `json:"unit,omitempty"`
}

// AskUserQuestionInput is the input schema.
type AskUserQuestionInput struct {
	// Questions is the list of 1-4 questions to ask.
	Questions []Question `json:"questions"`
	// Answers is an optional pre-populated answer map keyed by question text
	// (the host fills this in once the user responds).
	Answers map[string]StringOrList `json:"answers,omitempty"`
	// Metadata carries an optional analytics source identifier (not shown to
	// the user).
	Metadata *AskMetadata `json:"metadata,omitempty"`
}

// AskMetadata is the optional analytics metadata block.
type AskMetadata struct {
	Source string `json:"source,omitempty"`
}

// StringOrList is an answer value that is either a single label or a list of
// labels. An array is coerced into a comma-joined string on output.
type StringOrList struct {
	Values []string
}

// UnmarshalJSON accepts either a JSON string or an array of strings.
func (s *StringOrList) UnmarshalJSON(data []byte) error {
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		s.Values = []string{single}
		return nil
	}
	var list []string
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}
	s.Values = list
	return nil
}

// MarshalJSON emits a single comma-joined string (array -> "a, b, c").
func (s StringOrList) MarshalJSON() ([]byte, error) {
	return json.Marshal(strings.Join(s.Values, ", "))
}

// AskUserQuestionResult is the output schema (answers + annotations).
type AskUserQuestionResult struct {
	// Answers maps each question text to the user's selected label(s).
	Answers map[string]StringOrList `json:"answers,omitempty"`
	// FollowUp is set when the user asked for another round of questions.
	FollowUp bool `json:"followUp,omitempty"`
}

// AskUserQuestionTool implements the AskUserQuestion tool: prompt the user
// with one or more multiple-choice (or text/number) questions. The actual
// asking is performed by the host UI ("ask" permission behavior); this tool
// validates the schema and produces the structured request.
type AskUserQuestionTool struct{}

// Name returns the tool's protocol name.
func (AskUserQuestionTool) Name() protocol.Tool { return protocol.ToolAskUserQuestion }

// AskRequestQuestion is one normalized question in the structured host request.
type AskRequestQuestion struct {
	Question    string           `json:"question"`
	Header      string           `json:"header"`
	Options     []QuestionOption `json:"options"`
	MultiSelect bool             `json:"multiSelect"`
	Kind        string           `json:"kind,omitempty"`
}

// AskRequest is the structured prompt handed to the host UI.
type AskRequest struct {
	Tool      protocol.Tool        `json:"tool"`
	Questions []AskRequestQuestion `json:"questions"`
}

// Validate checks the schema constraints and cross-field rules:
//   - 1..4 questions;
//   - unique question texts;
//   - choice questions have 2..4 options with unique labels;
//   - text/number questions take no options and no multiSelect;
//   - number questions require numeric min < max, and defaultValue in range;
//   - header length within AskHeaderMaxChars.
func (AskUserQuestionTool) Validate(in AskUserQuestionInput) error {
	n := len(in.Questions)
	if n < AskMinQuestions || n > AskMaxQuestions {
		return fmt.Errorf("questions must have %d-%d items, got %d", AskMinQuestions, AskMaxQuestions, n)
	}

	seenQ := make(map[string]struct{}, n)
	for i := range in.Questions {
		q := in.Questions[i]
		if q.Question == "" {
			return errors.New("question text cannot be empty")
		}
		if _, dup := seenQ[q.Question]; dup {
			return errors.New("Question texts must be unique, option labels must be unique within each question")
		}
		seenQ[q.Question] = struct{}{}

		if len([]rune(q.Header)) > AskHeaderMaxChars {
			return fmt.Errorf("header %q exceeds %d chars", q.Header, AskHeaderMaxChars)
		}

		kind := q.Kind
		if kind == "" {
			kind = QuestionKindChoice
		}
		switch kind {
		case QuestionKindChoice:
			if len(q.Options) < AskMinOptions || len(q.Options) > AskMaxOptions {
				return fmt.Errorf("a choice question needs %d-%d options", AskMinOptions, AskMaxOptions)
			}
			seenL := make(map[string]struct{}, len(q.Options))
			for _, o := range q.Options {
				if _, dup := seenL[o.Label]; dup {
					return errors.New("Question texts must be unique, option labels must be unique within each question")
				}
				seenL[o.Label] = struct{}{}
			}
		case QuestionKindText:
			if len(q.Options) > 0 {
				return errors.New(`a "text" question takes no options`)
			}
			if q.MultiSelect {
				return errors.New(`multiSelect applies only to choice questions, not "text"`)
			}
		case QuestionKindNumber:
			if len(q.Options) > 0 {
				return errors.New(`a "number" question takes no options`)
			}
			if q.MultiSelect {
				return errors.New(`multiSelect applies only to choice questions, not "number"`)
			}
			if q.Min == nil || q.Max == nil || !(*q.Min < *q.Max) {
				return errors.New("A number question needs numeric min and max with min < max.")
			}
			if q.DefaultValue != nil && (*q.DefaultValue < *q.Min || *q.DefaultValue > *q.Max) {
				return errors.New("defaultValue must lie within min..max.")
			}
		default:
			return fmt.Errorf("invalid question kind %q", q.Kind)
		}
	}
	return nil
}

// Execute validates the input and returns the structured request for the host
// UI to present. The interactive answer is supplied out-of-band by the host:
// the toolrun adapter (AskUserQuestionAdapter) hands this request to the
// injected Asker, which drives the ui dialog, blocks until the user responds,
// and maps each answer back to its canonical option label (the ui ask
// round-trip reading result.answers[question]). In -p /
// non-interactive mode no Asker is wired, so the adapter returns the structured
// request without blocking. Execute itself only builds and validates the
// request; the blocking answer round-trip is host/UI territory.
func (t AskUserQuestionTool) Execute(in AskUserQuestionInput) (AskRequest, error) {
	if err := t.Validate(in); err != nil {
		return AskRequest{}, err
	}
	req := AskRequest{Tool: t.Name(), Questions: make([]AskRequestQuestion, 0, len(in.Questions))}
	for _, q := range in.Questions {
		opts := q.Options
		if opts == nil {
			opts = []QuestionOption{}
		}
		req.Questions = append(req.Questions, AskRequestQuestion{
			Question:    q.Question,
			Header:      q.Header,
			Options:     opts,
			MultiSelect: q.MultiSelect,
			Kind:        q.Kind,
		})
	}
	return req, nil
}
