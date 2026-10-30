package textutil

import (
	"math"
	"testing"
)

func TestNumberParsing(t *testing.T) {
	if v := ParseGroupedNumber("12"); v != 12 {
		t.Errorf("ParseGroupedNumber(12)=%v", v)
	}
	if v := ParseGroupedNumber("1,000"); v != 1000 {
		t.Errorf("ParseGroupedNumber(1,000)=%v", v)
	}
	if v := ParseGroupedNumber("abc"); !math.IsNaN(v) {
		t.Errorf("ParseGroupedNumber(abc)=%v want NaN", v)
	}
}
