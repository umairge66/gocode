//go:build !unix

package sandbox

import "os"

func ensurePrivateDir(dir string) error { return os.MkdirAll(dir, 0o700) }
