// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runCmd(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(args, strings.NewReader(stdin), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestCheckExampleConfig(t *testing.T) {
	code, out, errOut := runCmd(t, "", "-config", "../../configs/example.yaml", "-check")
	if code != 0 || !strings.Contains(out, "configuration OK") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestCheckBrokenConfigNamesKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte("routes: []\nlog:\n  level: loud\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := runCmd(t, "", "-config", path, "-check")
	if code != 1 || !strings.Contains(errOut, "log.level") || !strings.Contains(errOut, "routes") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
}

func TestHashToken(t *testing.T) {
	// SHA-256 of "secret".
	const want = "2bb80d537b1da3e38bd30361aa855686bde0eacd7162fef6a25fe97bf527a25b"
	code, out, _ := runCmd(t, "secret\n", "-hash-token")
	if code != 0 || strings.TrimSpace(out) != want {
		t.Fatalf("code %d, out %q", code, out)
	}
	if code, _, _ := runCmd(t, "\n", "-hash-token"); code == 0 {
		t.Fatal("empty token accepted")
	}
}

func TestMissingConfigFlag(t *testing.T) {
	if code, _, _ := runCmd(t, ""); code != 2 {
		t.Fatalf("code %d, want 2", code)
	}
}
