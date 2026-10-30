package pathguard

// This file holds the pure, filesystem-free part of the path-vetting
// engine: its *syntactic* classifier, built on the path-classification
// predicates in internal/pathclass. The walker-backed entry points are in
// realpath.go.

func isASCIILetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
