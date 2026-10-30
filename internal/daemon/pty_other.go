//go:build !linux && !darwin && !windows

package daemon

func startTerminal(TerminalSpec) (Terminal, error) { return nil, ErrPTYUnsupported }
