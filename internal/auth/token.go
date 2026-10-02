// SPDX-License-Identifier: Apache-2.0

// Package auth implements client authentication to the proxy.
package auth

import (
	"crypto/sha256"
	"encoding/hex"
)

// HashToken returns the hex SHA-256 of a proxy client token, the form stored
// in the tokens file.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
