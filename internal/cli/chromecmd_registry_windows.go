//go:build windows

package cli

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// winNativeHostRegistry writes the per-browser native-messaging host keys under
// HKEY_CURRENT_USER with golang.org/x/sys/windows/registry (no reg.exe child
// process, no admin rights needed).
type winNativeHostRegistry struct{}

func defaultNativeHostRegistry() nativeHostRegistry { return winNativeHostRegistry{} }

// splitHKCU turns `HKCU\Software\...` into the subkey path under
// registry.CURRENT_USER. Only HKCU keys are ever touched.
func splitHKCU(key string) (string, error) {
	const prefix = `HKCU\`
	if !strings.HasPrefix(key, prefix) || len(key) == len(prefix) {
		return "", fmt.Errorf("refusing to touch non-HKCU registry key %q", key)
	}
	return key[len(prefix):], nil
}

func (winNativeHostRegistry) SetDefault(key, value string) error {
	sub, err := splitHKCU(key)
	if err != nil {
		return err
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, sub, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue("", value)
}

func (winNativeHostRegistry) GetDefault(key string) (string, error) {
	sub, err := splitHKCU(key)
	if err != nil {
		return "", err
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, sub, registry.QUERY_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return "", errRegistryKeyAbsent
		}
		return "", err
	}
	defer k.Close()
	v, _, err := k.GetStringValue("")
	if errors.Is(err, registry.ErrNotExist) {
		return "", errRegistryKeyAbsent
	}
	return v, err
}

func (winNativeHostRegistry) DeleteKey(key string) error {
	sub, err := splitHKCU(key)
	if err != nil {
		return err
	}
	if err := registry.DeleteKey(registry.CURRENT_USER, sub); err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return errRegistryKeyAbsent
		}
		return err
	}
	return nil
}
