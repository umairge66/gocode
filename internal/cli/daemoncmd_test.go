package cli

import (
	"testing"
)

func TestParseDaemonArgs(t *testing.T) {
	cases := []struct {
		args     []string
		stdinTTY bool
		sub      string
		origin   string
		logPath  string
	}{
		{nil, false, "run", "", ""},
		{nil, true, "", "", ""},
		{[]string{"run"}, false, "run", "", ""},
		{[]string{"stop"}, true, "stop", "", ""},
		{[]string{"--origin", "service", "run"}, false, "run", "service", ""},
		{[]string{"--origin=transient", "stop"}, false, "stop", "transient", ""},
		{[]string{"run", "--log-file", "/x/d.log"}, false, "run", "", "/x/d.log"},
		{[]string{"run", "--spawned-by", "bg"}, false, "run", "", ""},
		{[]string{"frobnicate"}, false, "frobnicate", "", ""}, // unknown bare word -> that sub
	}
	for _, c := range cases {
		inv := parseDaemonArgs(c.args, c.stdinTTY)
		if inv.Sub != c.sub || inv.Origin != c.origin || inv.LogPath != c.logPath {
			t.Errorf("parseDaemonArgs(%v, tty=%v) = {sub=%q origin=%q log=%q} want {sub=%q origin=%q log=%q}",
				c.args, c.stdinTTY, inv.Sub, inv.Origin, inv.LogPath, c.sub, c.origin, c.logPath)
		}
	}
}
