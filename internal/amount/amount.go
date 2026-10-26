// Package amount validates the canonical decimal-string budget profile.
package amount

import "regexp"

var pattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,13})(\.[0-9]{0,5}[1-9])?$`)

func Valid(s string) bool { return pattern.MatchString(s) }
