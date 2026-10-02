// SPDX-License-Identifier: Apache-2.0

// Package auth implements client authentication to the proxy (doc/
// architecture.md, Client authentication). The client's upstream key is not
// handled here: it passes through untouched.
package auth

import (
	"bufio"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"

	"github.com/krauncher/krauncher-proxy/internal/config"
)

// HashToken returns the hex SHA-256 of a proxy client token, the form stored
// in the tokens file.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Failure reasons, used as metric label values.
const (
	ReasonMissing        = "missing"
	ReasonInvalid        = "invalid"
	ReasonCertNotAllowed = "cert_not_allowed"
)

// Authenticator checks a request and returns the client name. A nil
// *Authenticator accepts everything (client_auth.mode: off).
type Authenticator struct {
	mode    string
	header  string
	tokens  []token
	nameCN  bool
	allowed map[string]bool // mTLS; empty = any certificate the CA signed

	failures [3]atomic.Uint64 // by reason, in the order of reasons
}

type token struct {
	name string
	hash [sha256.Size]byte
}

var reasons = [...]string{ReasonMissing, ReasonInvalid, ReasonCertNotAllowed}

// New builds the authenticator for the configuration; nil when mode is off.
func New(c config.ClientAuth) (*Authenticator, error) {
	a := &Authenticator{mode: c.Mode, header: c.Header, nameCN: c.MTLS.NameFrom == "cn"}
	switch c.Mode {
	case config.AuthOff:
		return nil, nil
	case config.AuthHeader:
		toks, err := LoadTokens(c.TokensFile)
		if err != nil {
			return nil, err
		}
		a.tokens = toks
	case config.AuthMTLS:
		a.allowed = map[string]bool{}
		for _, n := range c.MTLS.AllowedNames {
			a.allowed[n] = true
		}
	}
	return a, nil
}

// LoadTokens reads "name:sha256hex" lines; blank lines and # comments are
// skipped. The file holds hashes only, never tokens.
func LoadTokens(path string) ([]token, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("client_auth.tokens_file: %w", err)
	}
	defer f.Close()
	var out []token
	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		s := strings.TrimSpace(sc.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		name, hexHash, ok := strings.Cut(s, ":")
		raw, err := hex.DecodeString(hexHash)
		if !ok || name == "" || err != nil || len(raw) != sha256.Size {
			return nil, fmt.Errorf("client_auth.tokens_file line %d: want name:sha256hex", line)
		}
		var t token
		t.name = name
		copy(t.hash[:], raw)
		out = append(out, t)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("client_auth.tokens_file: no tokens")
	}
	return out, nil
}

// Names returns the client names this authenticator can produce, for the
// metrics label allowlist.
func (a *Authenticator) Names() []string {
	if a == nil {
		return nil
	}
	var out []string
	for _, t := range a.tokens {
		out = append(out, t.name)
	}
	for n := range a.allowed {
		out = append(out, n)
	}
	return out
}

// Check authenticates r. On success it returns the client name and, in
// header mode, removes the token header so it never reaches the upstream.
func (a *Authenticator) Check(r *http.Request) (client, reason string, ok bool) {
	if a == nil {
		return "", "", true
	}
	switch a.mode {
	case config.AuthHeader:
		tok := r.Header.Get(a.header)
		r.Header.Del(a.header)
		if tok == "" {
			return a.fail(0)
		}
		sum := sha256.Sum256([]byte(tok))
		// Compare against every entry so timing does not depend on which
		// one matches.
		match := -1
		for i := range a.tokens {
			if subtle.ConstantTimeCompare(sum[:], a.tokens[i].hash[:]) == 1 {
				match = i
			}
		}
		if match < 0 {
			return a.fail(1)
		}
		return a.tokens[match].name, "", true
	case config.AuthMTLS:
		// The TLS layer has already verified the chain against the CA.
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			return a.fail(0)
		}
		cert := r.TLS.PeerCertificates[0]
		name := cert.Subject.CommonName
		if !a.nameCN {
			name = ""
			if len(cert.DNSNames) > 0 {
				name = cert.DNSNames[0]
			}
		}
		if name == "" || (len(a.allowed) > 0 && !a.allowed[name]) {
			return a.fail(2)
		}
		return name, "", true
	}
	return a.fail(1)
}

func (a *Authenticator) fail(i int) (string, string, bool) {
	a.failures[i].Add(1)
	return "", reasons[i], false
}

// Failures returns the failure count for a reason.
func (a *Authenticator) Failures(reason string) uint64 {
	if a == nil {
		return 0
	}
	for i, r := range reasons {
		if r == reason {
			return a.failures[i].Load()
		}
	}
	return 0
}

// Reasons lists the failure reasons.
func Reasons() []string { return reasons[:] }
