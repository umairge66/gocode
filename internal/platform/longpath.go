package platform

import "os"

// Getwd is os.Getwd with Windows 8.3 short names ("RUNNER~1") expanded to
// their long form, so paths built from the working directory are judged by
// the permission rules under the spelling the user sees.
func Getwd() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return wd, err
	}
	return LongPath(wd), nil
}
