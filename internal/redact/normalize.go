// Package redact implements the secret-redaction and Unicode-normalization
// engines: Unicode NFC/NFD/NFKC/NFKD normalization
// and invisible-character sanitization, grapheme/emoji helpers, the provider
// token secret-detection engine, and the
// free-text credential scanner.
//
// Patterns that need RE2-incompatible regex features
// (look-ahead / look-behind / back-references / \p{RGI_Emoji}) are expressed
// as equivalent RE2 forms or small hand-written scanners, and every such case is
// called out with a comment carrying the PCRE-style pattern.
package redact
