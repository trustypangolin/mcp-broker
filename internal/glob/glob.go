// Package glob implements the broker's single glob syntax, used for policy matches, process
// paths and connector selectors. '*' matches any run of characters, including '/' and '\',
// and '?' matches exactly one character. Matching is case-insensitive because tool names,
// Windows paths and connector names are all compared case-insensitively.
package glob

import "strings"

// Match reports whether s matches pattern.
func Match(pattern, s string) bool {
	p := []rune(strings.ToLower(pattern))
	t := []rune(strings.ToLower(s))

	// Iterative wildcard match with single-star backtracking: linear in practice and
	// immune to the exponential blowup of naive recursion on patterns like "*a*a*a*".
	pi, ti := 0, 0
	star, mark := -1, 0
	for ti < len(t) {
		switch {
		case pi < len(p) && (p[pi] == '?' || p[pi] == t[ti]):
			pi++
			ti++
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, ti
			pi++
		case star >= 0:
			pi = star + 1
			mark++
			ti = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}
