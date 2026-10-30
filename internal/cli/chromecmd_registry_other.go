//go:build !windows

package cli

import "errors"

// unsupportedNativeHostRegistry is the non-Windows registry seam. The chrome
// installer only consults the registry when goos == "windows", which on a real
// non-Windows build never happens; tests inject a fake instead.
type unsupportedNativeHostRegistry struct{}

func defaultNativeHostRegistry() nativeHostRegistry { return unsupportedNativeHostRegistry{} }

var errNoRegistry = errors.New("the Windows registry is not available on this OS")

func (unsupportedNativeHostRegistry) SetDefault(string, string) error   { return errNoRegistry }
func (unsupportedNativeHostRegistry) GetDefault(string) (string, error) { return "", errNoRegistry }
func (unsupportedNativeHostRegistry) DeleteKey(string) error            { return errNoRegistry }
