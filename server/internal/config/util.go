package config

import (
	"crypto/sha256"
	"regexp"
)

func reCompile(expr string) (*regexp.Regexp, error) { return regexp.Compile(expr) }

func deriveKey(pass string) []byte {
	sum := sha256.Sum256([]byte(pass))
	return sum[:]
}
