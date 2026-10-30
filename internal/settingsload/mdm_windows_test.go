//go:build windows

package settingsload

import (
	"testing"

	"golang.org/x/sys/windows/registry"
)

func TestWindowsRegistrySettingsRead(t *testing.T) {
	const sub = `SOFTWARE\gocode-test-policy`
	k, _, err := registry.CreateKey(registry.CURRENT_USER, sub, registry.ALL_ACCESS)
	if err != nil {
		t.Skipf("cannot create test key: %v", err)
	}
	defer registry.DeleteKey(registry.CURRENT_USER, sub)
	defer k.Close()

	if v, over := readRegistrySettingsAt(registry.CURRENT_USER, sub); v != nil || over {
		t.Fatalf("missing value: got %v %v", v, over)
	}
	if err := k.SetExpandStringValue(RegistryValueName, `{"model":"x"}`); err != nil {
		t.Fatal(err)
	}
	v, over := readRegistrySettingsAt(registry.CURRENT_USER, sub)
	if v == nil || over || *v != `{"model":"x"}` {
		t.Fatalf("got %v %v", v, over)
	}
	if err := k.SetDWordValue(RegistryValueName, 1); err != nil {
		t.Fatal(err)
	}
	if v, _ := readRegistrySettingsAt(registry.CURRENT_USER, sub); v != nil {
		t.Fatalf("non-string value must read as absent, got %q", *v)
	}
}

func TestWindowsPlatformReaderRuns(t *testing.T) {
	r := readPlatformOSPolicy()
	if r.IsWSL || len(r.Plists) != 0 {
		t.Fatalf("native Windows readout must carry registry fields only: %+v", r)
	}
}
