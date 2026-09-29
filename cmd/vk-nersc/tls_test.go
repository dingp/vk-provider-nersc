package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"os"
	"path/filepath"
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

// Kubernetes Secret volumes rotate ..data, leaving the mounted file symlinks
// unchanged. Exercise the actual callback and file layout used by handshakes.
func TestMountedCertificateRotation(t *testing.T) {
	dir := t.TempDir()
	writePair := func(name string, cert tls.Certificate) {
		t.Helper()
		target := filepath.Join(dir, name)
		if err := os.Mkdir(target, 0700); err != nil {
			t.Fatal(err)
		}
		certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
		keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
		if err != nil {
			t.Fatal(err)
		}
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
		for file, data := range map[string][]byte{"tls.crt": certPEM, "tls.key": keyPEM} {
			if err := os.WriteFile(filepath.Join(target, file), data, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	rotate := func(name string) {
		t.Helper()
		next := filepath.Join(dir, "..next")
		if err := os.Symlink(name, next); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(next, filepath.Join(dir, "..data")); err != nil {
			t.Fatal(err)
		}
	}
	first, err := selfSignedServingCert("test", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := selfSignedServingCert("test", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	writePair("version1", first)
	writePair("version2", second)
	rotate("version1")
	for _, file := range []string{"tls.crt", "tls.key"} {
		if err := os.Symlink(filepath.Join("..data", file), filepath.Join(dir, file)); err != nil {
			t.Fatal(err)
		}
	}
	caFile := filepath.Join(dir, "client-ca.crt")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: first.Certificate[0]}), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VK_TLS_CERT_FILE", filepath.Join(dir, "tls.crt"))
	t.Setenv("VK_TLS_KEY_FILE", filepath.Join(dir, "tls.key"))
	t.Setenv("VK_TLS_CLIENT_CA_FILE", caFile)
	t.Setenv("VK_TLS_CLIENT_COMMON_NAME", "kube-apiserver")
	config, err := kubeletTLSConfig("test", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if config.ClientAuth != tls.RequireAndVerifyClientCert || config.ClientCAs == nil || config.VerifyConnection == nil {
		t.Fatal("external TLS must authenticate clients")
	}
	for i, want := range []tls.Certificate{first, second} {
		if i == 1 {
			rotate("version2")
		}
		got, err := config.GetCertificate(&tls.ClientHelloInfo{})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got.Certificate[0], want.Certificate[0]) {
			t.Fatalf("rotation %d served stale certificate", i)
		}
	}
	mismatched := second
	mismatched.PrivateKey = first.PrivateKey
	writePair("mismatched", mismatched)
	rotate("mismatched")
	if _, err := config.GetCertificate(&tls.ClientHelloInfo{}); err == nil {
		t.Fatal("mismatched certificate and key accepted")
	}
	if err := os.WriteFile(filepath.Join(dir, "mismatched", "tls.crt"), []byte("not PEM"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.GetCertificate(&tls.ClientHelloInfo{}); err == nil {
		t.Fatal("malformed rotated certificate accepted")
	}
	rotate("version2")
	if _, err := config.GetCertificate(&tls.ClientHelloInfo{}); err != nil {
		t.Fatalf("valid rotation did not recover: %v", err)
	}
}
