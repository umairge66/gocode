package toolrun

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/tools/filetools"
)

// Every Grep option the tool accepts must be advertised to the model; a hidden
// option (e.g. -i) makes the model fall back to `grep -r -i` in Bash.
func TestGrepSchemaAdvertisesEveryInput(t *testing.T) {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(toolInputSchemas[protocol.ToolGrep], &schema); err != nil {
		t.Fatal(err)
	}
	typ := reflect.TypeOf(filetools.GrepInput{})
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		if _, ok := schema.Properties[name]; !ok {
			t.Errorf("Grep input %q is not in the advertised schema", name)
		}
	}
}

// Glob and Grep send the full descriptions (which teach "**" and
// steer away from Bash grep), not one-line summaries.
func TestSearchToolDescriptionsAreFull(t *testing.T) {
	if d := toolDescriptions[protocol.ToolGlob]; !strings.Contains(d, `"**/*.js"`) {
		t.Errorf("Glob description lacks the ** guidance: %q", d)
	}
	if d := toolDescriptions[protocol.ToolGrep]; !strings.Contains(d, "NEVER invoke `grep` or `rg` as a Bash command") {
		t.Errorf("Grep description lacks the Bash steering: %q", d)
	}
}
