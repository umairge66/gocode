package pluginmgr

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// copyTree copies the directory tree rooted at src into dst (creating dst),
// copying regular files and recreating directories. Symlinks and other
// non-regular files are skipped, so a materialized plugin never drags a link
// pointing outside its tree into the cache. dst is removed first so a reinstall
// is a clean replace.
func copyTree(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("pluginmgr: stat source %s: %w", src, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("pluginmgr: source %s is not a directory", src)
	}
	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("pluginmgr: clear %s: %w", dst, err)
	}
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case fi.IsDir():
			return os.MkdirAll(target, 0o755)
		case fi.Mode().IsRegular():
			return copyFile(p, target, fi.Mode().Perm())
		default:
			// Skip symlinks / devices / sockets.
			return nil
		}
	})
}

func copyFile(src, dst string, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
