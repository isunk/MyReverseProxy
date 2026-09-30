package ca

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/isunk/MyReverseProxy/internal/logging"
	"github.com/isunk/MyReverseProxy/internal/testutil"
)

func init() {
	logging.SetOutput(io.Discard)
}

func TestAuthority_SignsForSNI(t *testing.T) {
	caCert, caKey := testutil.AuthorityCA(t)
	authority := New(caCert, caKey)

	hello := &tls.ClientHelloInfo{ServerName: "api.example.com"}
	cert, err := authority.GetCertificate(hello)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "api.example.com" {
		t.Fatalf("SAN 应为 api.example.com，got %v", leaf.DNSNames)
	}
	if err := leaf.CheckSignatureFrom(caCert); err != nil {
		t.Fatalf("证书应由 CA 签发：%v", err)
	}

	cached, err := authority.GetCertificate(hello)
	if err != nil {
		t.Fatal(err)
	}
	if cached != cert {
		t.Fatal("同一 SNI 应命中缓存返回同一证书")
	}

	// 混合大小写 SNI 应归一化为小写，复用缓存
	mixed, err := authority.GetCertificate(&tls.ClientHelloInfo{ServerName: "API.Example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if mixed != cert {
		t.Fatal("混合大小写 SNI 应复用同一缓存证书")
	}

	if _, err := authority.GetCertificate(&tls.ClientHelloInfo{ServerName: ""}); err == nil {
		t.Fatal("缺少 SNI 应报错")
	}
}

func TestAuthority_ClearCache(t *testing.T) {
	caCert, caKey := testutil.AuthorityCA(t)
	authority := New(caCert, caKey)
	hello := &tls.ClientHelloInfo{ServerName: "api.example.com"}

	cert, err := authority.GetCertificate(hello)
	if err != nil {
		t.Fatal(err)
	}
	if cached, _ := authority.GetCertificate(hello); cached != cert {
		t.Fatal("同一 SNI 应命中缓存返回同一证书")
	}
	authority.ClearCache()
	renewed, err := authority.GetCertificate(hello)
	if err != nil {
		t.Fatal(err)
	}
	if renewed == cert {
		t.Fatal("ClearCache 后应重新签发证书，而非复用旧缓存")
	}
}

func TestAuthority_ConcurrentSameSNI(t *testing.T) {
	caCert, caKey := testutil.AuthorityCA(t)
	authority := New(caCert, caKey)
	hello := &tls.ClientHelloInfo{ServerName: "api.example.com"}

	const n = 8
	results := make([]*tls.Certificate, n)
	var start, done sync.WaitGroup
	start.Add(1)
	for i := range n {
		done.Add(1)
		go func(i int) {
			defer done.Done()
			start.Wait()
			cert, err := authority.GetCertificate(hello)
			if err != nil {
				t.Errorf("GetCertificate: %v", err)
				return
			}
			results[i] = cert
		}(i)
	}
	start.Done()
	done.Wait()
	for i := 1; i < n; i++ {
		if results[i] != results[0] {
			t.Fatal("同域名并发握手应合并为一次签发，复用同一证书")
		}
	}
}

func TestAuthority_ConcurrentDistinctSNI(t *testing.T) {
	caCert, caKey := testutil.AuthorityCA(t)
	authority := New(caCert, caKey)

	const n = 16
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			hello := &tls.ClientHelloInfo{ServerName: fmt.Sprintf("host%d.example.com", i)}
			cert, err := authority.GetCertificate(hello)
			if err != nil || cert == nil {
				t.Errorf("sign host%d: cert=%v err=%v", i, cert, err)
			}
		}(i)
	}
	wg.Wait()
}

func TestLoad_RejectsNonCA(t *testing.T) {
	cert := testutil.SelfSignedCert(t, []string{"api.example.com"})
	certPath := testutil.ConfigFile(t, "ca.crt", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})))
	key, err := x509.MarshalECPrivateKey(cert.PrivateKey.(*ecdsa.PrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	keyPath := testutil.ConfigFile(t, "ca.key", string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: key})))
	if _, _, err := Load(certPath, keyPath, true); err == nil {
		t.Fatal("want error for non-CA certificate")
	}
}

func TestLoad_Defaults(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")
	cfg, _, err := Load(certPath, keyPath, false)
	if err != nil || cfg != nil {
		t.Fatalf("want nil config without certs, got %v err=%v", cfg, err)
	}
	if err := os.WriteFile(certPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(certPath, keyPath, false); err == nil {
		t.Fatal("want error for missing key")
	}
}

func TestLoad_ExplicitPairRequiresBothFiles(t *testing.T) {
	cert := testutil.SelfSignedCert(t, []string{"api.example.com"})
	certPath := testutil.ConfigFile(t, "ca.crt", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})))
	cfg, _, err := Load(certPath, "/nonexistent/ca.key", true)
	if err == nil {
		t.Fatalf("want error when the explicit key file is missing, got %v", cfg)
	}
}
