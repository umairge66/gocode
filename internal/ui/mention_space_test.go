package ui

import "testing"

func mentionComposer(v string, cur int) ViewState {
	return ViewState{Composer: Composer{Value: v, Cursor: cur}}
}

// TestFileMentionSingleSpace: completing a file adds one trailing space, and
// a space typed right after it is not doubled; other typing is unaffected.
func TestFileMentionSingleSpace(t *testing.T) {
	vs := completeFileMention(mentionComposer("see @src/ca", 11), FileCandidate{Path: "src/calc.py"})
	if vs.Composer.Value != "see @src/calc.py " || vs.Composer.Cursor != 17 {
		t.Fatalf("after completion: %+v", vs.Composer)
	}
	typed := Reduce(vs, SetComposer{Composer: Composer{Value: "see @src/calc.py  ", Cursor: 18}})
	if typed.Composer.Value != "see @src/calc.py " || typed.Composer.Cursor != 17 {
		t.Fatalf("space after completion doubled: %+v", typed.Composer)
	}
	// The next space is a real one.
	again := Reduce(typed, SetComposer{Composer: Composer{Value: "see @src/calc.py  ", Cursor: 18}})
	if again.Composer.Value != "see @src/calc.py  " {
		t.Fatalf("second space dropped: %+v", again.Composer)
	}
	word := Reduce(vs, SetComposer{Composer: Composer{Value: "see @src/calc.py n", Cursor: 18}})
	if word.Composer.Value != "see @src/calc.py n" {
		t.Fatalf("typing after completion: %+v", word.Composer)
	}
}

// TestFileMentionReusesFollowingSpace: a mention completed in the middle of
// the text reuses the space after it.
func TestFileMentionReusesFollowingSpace(t *testing.T) {
	vs := completeFileMention(mentionComposer("@src/ca name it", 7), FileCandidate{Path: "src/calc.py"})
	if vs.Composer.Value != "@src/calc.py name it" || vs.Composer.Cursor != 13 {
		t.Fatalf("completion = %+v", vs.Composer)
	}
	dir := completeFileMention(mentionComposer("@sr", 3), FileCandidate{Path: "src", IsDir: true})
	if dir.Composer.Value != "@src/" || dir.mentionSpace != 0 {
		t.Fatalf("directory = %+v", dir.Composer)
	}
}
