package registry

import (
	"strings"
)

// TLD is the last label of name, already lower-case ASCII.
func TLD(name string) string {
	s := strings.TrimSuffix(strings.ToLower(name), ".")
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		return s[i+1:]
	}
	return s
}
