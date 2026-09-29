package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// Mounted Secret certificates are reloaded on each handshake, allowing rotation
// without restarting the provider and losing its in-memory job mappings.
func kubeletTLSConfig(nodeName, nodeAddress string) (*tls.Config, error) {
	config := &tls.Config{MinVersion: tls.VersionTLS12}
	certFile, keyFile := os.Getenv("VK_TLS_CERT_FILE"), os.Getenv("VK_TLS_KEY_FILE")
	caFile, clientName := os.Getenv("VK_TLS_CLIENT_CA_FILE"), os.Getenv("VK_TLS_CLIENT_COMMON_NAME")
	if certFile == "" && keyFile == "" && caFile == "" && clientName == "" {
		cert, err := selfSignedServingCert(nodeName, nodeAddress)
		if err != nil {
			return nil, err
		}
		config.Certificates = []tls.Certificate{cert}
		config.ClientAuth = tls.RequestClientCert
		return config, nil
	}
	if certFile == "" || keyFile == "" || caFile == "" || clientName == "" {
		return nil, fmt.Errorf("external kubelet TLS requires certificate, key, client CA and client common name")
	}
	if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
		return nil, err
	}
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("invalid kubelet client CA")
	}
	config.ClientCAs = pool
	config.ClientAuth = tls.RequireAndVerifyClientCert
	config.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		return &cert, err
	}
	config.VerifyConnection = func(state tls.ConnectionState) error {
		return verifyKubeletClient(state, clientName)
	}
	return config, nil
}

func verifyKubeletClient(state tls.ConnectionState, expected string) error {
	if len(state.VerifiedChains) == 0 || len(state.VerifiedChains[0]) == 0 ||
		state.VerifiedChains[0][0].Subject.CommonName != expected {
		return fmt.Errorf("kubelet client identity is not authorized")
	}
	return nil
}
