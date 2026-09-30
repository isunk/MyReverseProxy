package testutil

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
	"testing"
	"time"
)

// SelfSignedCert 生成单张自签服务器证书，域名写入 SAN，本机回环地址恒在其中。
func SelfSignedCert(t *testing.T, domains []string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: domains[0]},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		DNSNames:     domains,
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// AuthorityCA 生成自签 CA 对，供 MITM 签发与客户端信任根校验使用。
func AuthorityCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return caCert, key
}

// WriteCertFiles 把 CA 对写成 PEM 文件，返回证书与私钥路径。
func WriteCertFiles(t *testing.T, caCert *x509.Certificate, caKey *ecdsa.PrivateKey) (string, string) {
	t.Helper()
	certPath := ConfigFile(t, "ca.crt", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCert.Raw})))
	key, err := x509.MarshalECPrivateKey(caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := ConfigFile(t, "ca.key", string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: key})))
	return certPath, keyPath
}
