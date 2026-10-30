// Package textutil holds text-processing helpers: UTF-16-aware
// truncation and well-formedness (strings.go), message tag constants, wrapper-
// tag stripping and system-reminder parsing (tags.go), the "pasted content"
// block-marker parser (pasteblocks.go), the 32-bit string hash (hash.go),
// HTML/JSON/control-character escaping and label/path validation guards
// (sanitize.go), flexible number parsing (numbers.go), language-name
// normalization (language.go) and lodash snakeCase (snakecase.go).
//
// Unicode note: several helpers are specified in terms of UTF-16 code units
// (what JavaScript's String.prototype.length and slice() measure). Go strings
// are UTF-8, so these helpers operate on runes (code points) or bytes,
// whichever matches the intent; where the code-unit count matters
// (HeadUnits/TailUnits, StringHash, the length guards) UTF-16 behaviour is
// computed via unicode/utf16.
package textutil
