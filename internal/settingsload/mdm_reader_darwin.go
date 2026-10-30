//go:build darwin

package settingsload

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
)

// readPlatformOSPolicy reads the macOS managed-preferences plists in-process
// (no converter subprocess). See MacManagedPlistPaths for the paths.
func readPlatformOSPolicy() OSPolicyReadout {
	name := ""
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	return OSPolicyReadout{Plists: plistDocumentsFrom(MacManagedPlistPaths(name), readCapped, func(p string) bool {
		_, err := os.Stat(p)
		return !errors.Is(err, fs.ErrNotExist)
	})}
}

// readCapped reads a file up to the plist cap.
func readCapped(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxPlistBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxPlistBytes {
		return nil, fmt.Errorf("the document exceeds %d MiB", maxPlistBytes/1048576)
	}
	return data, nil
}
