// SPDX-License-Identifier: Apache-2.0

package harden

import (
	"fmt"
	"os"
)

// LooseSecretFile reports why a file holding a secret is readable by others
// (group or world permissions), or "" when it is not. A missing file is not
// reported here; the code that opens it fails with a clearer error.
func LooseSecretFile(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return ""
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Sprintf("mode %04o, expected no group or other access (0600)", perm)
	}
	return ""
}
