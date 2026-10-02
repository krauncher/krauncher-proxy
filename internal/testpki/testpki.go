// SPDX-License-Identifier: Apache-2.0

// Package testpki issues throwaway certificates for tests: a CA, server
// certificates for 127.0.0.1, client certificates.
package testpki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// PKI is a throwaway CA with helpers to issue server and client certificates.
type PKI struct {
	Dir    string
	CAFile string
	Pool   *x509.CertPool
	ca     *x509.Certificate
	caKey  *ecdsa.PrivateKey
}

func New(t testing.TB) *PKI {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	ca, _ := x509.ParseCertificate(der)
	p := &PKI{Dir: t.TempDir(), ca: ca, caKey: key, Pool: x509.NewCertPool()}
	p.Pool.AddCert(ca)
	p.CAFile = p.write("ca.pem", "CERTIFICATE", der)
	return p
}

func (p *PKI) write(name, typ string, der []byte) string {
	path := filepath.Join(p.Dir, name)
	os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600)
	return path
}

// Issue returns a certificate and key signed by the CA, as files and as a
// tls.Certificate.
func (p *PKI) Issue(t testing.TB, name string, server bool) (certFile, keyFile string, c tls.Certificate) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature,
	}
	if server {
		tmpl.ExtKeyUsage, tmpl.IPAddresses = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, []net.IP{net.ParseIP("127.0.0.1")}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.ca, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalECPrivateKey(key)
	certFile, keyFile = p.write(name+".pem", "CERTIFICATE", der), p.write(name+".key", "EC PRIVATE KEY", kder)
	c, err = tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile, c
}

// Client trusts the CA and presents the given client certificates.
func (p *PKI) Client(certs ...tls.Certificate) *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: p.Pool, Certificates: certs}}}
}
