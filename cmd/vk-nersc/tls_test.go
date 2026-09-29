package main

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"testing"
)

func TestVerifyKubeletClientRequiresVerifiedExpectedIdentity(t *testing.T) {
	apiCert := &x509.Certificate{Subject: pkix.Name{CommonName: "kube-apiserver"}}
	adminCert := &x509.Certificate{Subject: pkix.Name{CommonName: "admin"}}
	for _, tc := range []struct {
		name    string
		state   tls.ConnectionState
		allowed bool
	}{
		{"anonymous", tls.ConnectionState{}, false},
		{"unverified", tls.ConnectionState{PeerCertificates: []*x509.Certificate{apiCert}}, false},
		{"other trusted client", tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{adminCert}}}, false},
		{"api server", tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{apiCert}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyKubeletClient(tc.state, "kube-apiserver")
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v error=%v", tc.allowed, err)
			}
		})
	}
}

func TestExternalTLSFailsClosedOnPartialConfiguration(t *testing.T) {
	t.Setenv("VK_TLS_CERT_FILE", "/missing/tls.crt")
	t.Setenv("VK_TLS_KEY_FILE", "")
	if _, err := kubeletTLSConfig("test", "127.0.0.1"); err == nil {
		t.Fatal("partial external TLS must not fall back to unauthenticated self-signed serving")
	}
}
