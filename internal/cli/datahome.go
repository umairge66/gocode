package cli

import (
	"fmt"
	"sync"

	"github.com/umairge66/gocode/internal/confighome"
)

// dataHomeNotice holds the one-line message of a data move done by this
// process, so an interactive session can also show it as a system notice
// (the terminal UI may take over the screen before stderr is read).
var dataHomeNotice struct {
	sync.Mutex
	text string
}

// migrateDataHome moves user data left in ~/.gocode and ~/.gocode.json into
// the gocode root (confighome.Migrate) before anything reads it. Run calls it
// for every invocation that may touch user data; once the move is done it
// costs a single stat.
func migrateDataHome(env *Env) {
	get := envGetenv(env)
	home := trustHome(env)
	if home == "" {
		return
	}
	root, ok := confighome.Resolve(get, home)
	if !ok {
		return
	}
	res := confighome.Migrate(confighome.MigrateOptions{
		Root:       root,
		Home:       home,
		Overridden: confighome.Overridden(get),
	})
	if res.Notice == "" {
		return
	}
	if env.Stderr != nil {
		fmt.Fprintln(env.Stderr, res.Notice)
	}
	dataHomeNotice.Lock()
	dataHomeNotice.text = res.Notice
	dataHomeNotice.Unlock()
}

// takeDataHomeNotice returns the pending data-move notice once.
func takeDataHomeNotice() string {
	dataHomeNotice.Lock()
	defer dataHomeNotice.Unlock()
	t := dataHomeNotice.text
	dataHomeNotice.text = ""
	return t
}
