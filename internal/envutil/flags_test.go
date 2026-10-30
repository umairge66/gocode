package envutil

import (
	"testing"
)

func TestEnvTruthy(t *testing.T) {
	truthy := []string{"1", "true", "yes", "on", "TRUE", "Yes", "ON", " on ", "  true  "}
	for _, v := range truthy {
		if !EnvTruthy(v) {
			t.Errorf("EnvTruthy(%q) = false, want true", v)
		}
	}
	falsy := []string{"", " ", "0", "false", "no", "off", "2", "truex", "enabled", "y", "random"}
	for _, v := range falsy {
		if EnvTruthy(v) {
			t.Errorf("EnvTruthy(%q) = true, want false", v)
		}
	}
}

func TestEnvFalsy(t *testing.T) {
	falsyTrue := []string{"0", "false", "no", "off", "FALSE", "Off", " no ", "  0  "}
	for _, v := range falsyTrue {
		if !EnvFalsy(v) {
			t.Errorf("EnvFalsy(%q) = false, want true", v)
		}
	}
	notFalsy := []string{"", " ", "1", "true", "yes", "on", "nope", "disabled", "random"}
	for _, v := range notFalsy {
		if EnvFalsy(v) {
			t.Errorf("EnvFalsy(%q) = true, want false", v)
		}
	}
}
