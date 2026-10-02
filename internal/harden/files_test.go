// SPDX-License-Identifier: Apache-2.0

package harden

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLooseSecretFile(t *testing.T) {
	dir := t.TempDir()
	for mode, loose := range map[os.FileMode]bool{0o600: false, 0o400: false, 0o640: true, 0o644: true, 0o604: true} {
		p := filepath.Join(dir, mode.String())
		os.WriteFile(p, []byte("x"), mode)
		os.Chmod(p, mode)
		if got := LooseSecretFile(p) != ""; got != loose {
			t.Errorf("%04o: loose=%v, want %v", mode, got, loose)
		}
	}
	if LooseSecretFile(filepath.Join(dir, "missing")) != "" {
		t.Error("missing file reported")
	}
}
