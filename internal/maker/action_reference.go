package maker

import "regexp"

// serverReferencePattern binds the hash and exported action name to one call.
// Only the observed identifier/member arguments and void 0 are supported; in
// particular quotes and parentheses cannot bridge neighboring chained calls.
// names is a literal or a fixed regular-expression alternative, never user JS.
func serverReferencePattern(names string) *regexp.Regexp {
	const identifier = `[A-Za-z_$][A-Za-z0-9_$]{0,100}`
	const member = identifier + `(?:\.` + identifier + `){0,3}`
	const argument = `(?:void\s+0|` + member + `)`
	return regexp.MustCompile(`\bcreateServerReference\)\s*\(\s*"([a-fA-F0-9]{40,})"\s*,\s*` + argument + `(?:\s*,\s*` + argument + `){0,2}\s*,\s*"` + names + `"\s*\)`)
}
