//go:build windows

package settingsload

import (
	"errors"

	"golang.org/x/sys/windows/registry"
)

// readPlatformOSPolicy reads the "Settings" value of the HKLM and HKCU
// policy keys directly through the registry API (no reg.exe subprocess).
func readPlatformOSPolicy() OSPolicyReadout {
	var r OSPolicyReadout
	v, oversize := readRegistrySettings(registry.LOCAL_MACHINE)
	if v != nil {
		r.HKLM = v
	} else if oversize {
		r.HKLMUnreadReason = registryUnreadOversize
	}
	r.HKCU, _ = readRegistrySettings(registry.CURRENT_USER)
	return r
}

// readRegistrySettings reads the REG_SZ / REG_EXPAND_SZ "Settings" value
// (unexpanded, as reg query prints it). A missing key/value or any read error
// is nil; a value over 2 MiB reports
// oversize.
func readRegistrySettings(root registry.Key) (*string, bool) {
	return readRegistrySettingsAt(root, RegistrySubkey)
}

// readRegistrySettingsAt is readRegistrySettings for an explicit subkey (a
// test seam).
func readRegistrySettingsAt(root registry.Key, subkey string) (*string, bool) {
	k, err := registry.OpenKey(root, subkey, registry.QUERY_VALUE)
	if err != nil {
		return nil, false
	}
	defer k.Close()
	n, typ, err := k.GetValue(RegistryValueName, nil)
	if err != nil && !errors.Is(err, registry.ErrShortBuffer) {
		return nil, false
	}
	if typ != registry.SZ && typ != registry.EXPAND_SZ {
		return nil, false
	}
	// UTF-16 bytes; the reg.exe output cap was on the decoded text.
	if n/2 > registryMaxBytes {
		return nil, true
	}
	s, _, err := k.GetStringValue(RegistryValueName)
	if err != nil {
		return nil, false
	}
	return &s, false
}
