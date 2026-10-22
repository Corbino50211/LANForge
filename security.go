package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func ensureCertificate(dir string) (string, error) {
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")
	if b, e := os.ReadFile(certPath); e == nil {
		block, _ := pem.Decode(b)
		if block == nil {
			return "", errors.New("invalid saved TLS certificate")
		}
		if _, e = tls.LoadX509KeyPair(certPath, keyPath); e != nil {
			return "", e
		}
		sum := sha256.Sum256(block.Bytes)
		return hex.EncodeToString(sum[:]), nil
	}
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		return "", e
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	ips := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		ip, _, e := net.ParseCIDR(a.String())
		if e == nil && ip.IsPrivate() {
			ips = append(ips, ip)
		}
	}
	hostname, _ := os.Hostname()
	tmpl := x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "LANForge Local Controller"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: []string{"localhost", hostname}, IPAddresses: ips}
	der, e := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if e != nil {
		return "", e
	}
	pk, e := x509.MarshalECPrivateKey(key)
	if e != nil {
		return "", e
	}
	if e = atomicWrite(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: pk}), 0600); e != nil {
		return "", e
	}
	if e = atomicWrite(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); e != nil {
		return "", e
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:]), nil
}
func pinnedTLS(fingerprint string) (*tls.Config, error) {
	fp := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(fingerprint), ":", ""))
	expected, e := hex.DecodeString(fp)
	if e != nil || len(expected) != 32 {
		return nil, errors.New("copy the complete certificate fingerprint from the controller's Add computer screen")
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true, VerifyConnection: func(s tls.ConnectionState) error {
		if len(s.PeerCertificates) == 0 {
			return errors.New("controller did not present a certificate")
		}
		cert := s.PeerCertificates[0]
		actual := sha256.Sum256(cert.Raw)
		if subtle.ConstantTimeCompare(actual[:], expected) != 1 {
			return errors.New("controller certificate does not match the pairing fingerprint")
		}
		if time.Now().Before(cert.NotBefore) || time.Now().After(cert.NotAfter) {
			return errors.New("controller certificate has expired or is not yet valid")
		}
		return nil
	}}, nil
}
