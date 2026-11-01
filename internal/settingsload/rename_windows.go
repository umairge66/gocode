package settingsload

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// replaceFile renames tmp over path. Windows refuses to replace a file that
// another process has open at that moment (an editor, another gocode
// session reading its settings), so a refusal is retried for a short while.
func replaceFile(tmp, path string) error {
	var err error
	for delay := 5 * time.Millisecond; delay < time.Second; delay *= 2 {
		if err = os.Rename(tmp, path); err == nil || !transientRenameError(err) {
			return err
		}
		time.Sleep(delay)
	}
	return err
}

func transientRenameError(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
