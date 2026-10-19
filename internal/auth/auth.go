// Package auth checks the static bearer tokens that guard the proxy's client
// and admin endpoints.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"strings"
)

// BearerMatches reports whether an Authorization header carries the bearer
// token. Both sides are hashed first so the comparison takes the same time
// whatever the header's length or content.
func BearerMatches(header, token string) bool {
	const prefix = "bearer "
	presented := ""
	if len(header) > len(prefix) && strings.EqualFold(header[:len(prefix)], prefix) {
		presented = strings.TrimSpace(header[len(prefix):])
	}
	presentedSum := sha256.Sum256([]byte(presented))
	tokenSum := sha256.Sum256([]byte(token))
	match := subtle.ConstantTimeCompare(presentedSum[:], tokenSum[:]) == 1
	return match && presented != "" && token != ""
}
