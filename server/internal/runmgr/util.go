package runmgr

import "crypto/sha256"

func sha256Sum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
