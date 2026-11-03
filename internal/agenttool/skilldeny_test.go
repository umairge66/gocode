package agenttool

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A Skill(<name>) deny rule (Registry.Denied) refuses the skill the person
// types, not only the model's call.
func TestSkillInvokeAsUserDenied(t *testing.T) {
	s, _ := skillFixture(t)
	s.Registry.Denied = func(name string) bool { return name == "secret" }
	_, err := s.InvokeAsUser(context.Background(), "secret", "")
	if !errors.Is(err, ErrSkillDenied) || !strings.Contains(err.Error(), "Skill /secret is blocked by a permission deny rule (Skill(secret))") {
		t.Fatalf("err = %v, want ErrSkillDenied", err)
	}
	s.Registry.Denied = func(string) bool { return false }
	if _, err := s.InvokeAsUser(context.Background(), "secret", ""); err != nil {
		t.Fatalf("not denied: %v", err)
	}
}
