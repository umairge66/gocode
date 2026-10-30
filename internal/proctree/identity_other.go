//go:build !unix && !windows

package proctree

func startTime(int) string { return "" }
func ownedBySelf(int) bool { return false }
