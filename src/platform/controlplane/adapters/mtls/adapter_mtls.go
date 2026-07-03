package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────
// adapter_mtls.go — MTLSPort 인메모리 구현체
//
// 설계 원칙 (PAYMENT_AUTH_DESIGN_DECISIONS.md §4):
//   - Edge ↔ Core 간, 마이크로서비스 간 통신에 사용.
//   - 클라이언트와 서버 모두 인증서를 제시하여 상호 인증.
//   - 인증서 유효 기간: 24시간 (자동 갱신).
//   - 폐기된 인증서는 revoked 맵에 등록하여 검증 시 차단.
// ─────────────────────────────────────────────────────────────────────────

// certRecord — 발급된 인증서 기록.
type certRecord struct {
	Identity  MTLSIdentity
	CertPEM   []byte
	KeyPEM    []byte
	Revoked   bool
}

// MTLSAdapter — MTLSPort 인메모리 구현체.
type MTLSAdapter struct {
	mu      sync.RWMutex
	certs   map[string]*certRecord // fingerprint(CN+OrgID+IssuedAt) → record
	caKey   *rsa.PrivateKey        // 내부 CA 키 (테스트용 자체 서명)
	caCert  *x509.Certificate      // 내부 CA 인증서
}

// 컴파일 타임 인터페이스 계약 검증.
var _ MTLSPort = (*MTLSAdapter)(nil)

// NewMTLSAdapter — MTLSAdapter 생성 (테스트용 자체 서명 CA 포함).
func NewMTLSAdapter() (*MTLSAdapter, error) {
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("mtls: CA key generation failed: %w", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName:   "sovereign-internal-ca",
			Organization: []string{"sovereign-core"},
		},
		NotBefore:             time.Now().Add(-1 * time.Minute),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	caCertDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("mtls: CA cert creation failed: %w", err)
	}
	caCert, err := x509.ParseCertificate(caCertDER)
	if err != nil {
		return nil, fmt.Errorf("mtls: CA cert parse failed: %w", err)
	}
	return &MTLSAdapter{
		certs:  make(map[string]*certRecord),
		caKey:  caKey,
		caCert: caCert,
	}, nil
}

func (a *MTLSAdapter) IssueServiceCert(ctx context.Context, serviceName, orgID string) (certPEM, keyPEM []byte, err error) {
	if serviceName == "" {
		return nil, nil, fmt.Errorf("mtls: serviceName is required")
	}
	if orgID == "" {
		return nil, nil, fmt.Errorf("mtls: orgID is required")
	}

	// 서비스 키 생성.
	svcKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("mtls: service key generation failed: %w", err)
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(now.UnixNano()),
		Subject: pkix.Name{
			CommonName:         serviceName,
			Organization:       []string{orgID},
			OrganizationalUnit: []string{"sovereign-service"},
		},
		NotBefore: now.Add(-1 * time.Minute),
		NotAfter:  now.Add(24 * time.Hour), // 24시간 유효
		KeyUsage:  x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageClientAuth,
			x509.ExtKeyUsageServerAuth,
		},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, a.caCert, &svcKey.PublicKey, a.caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("mtls: cert creation failed: %w", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(svcKey)})

	identity := MTLSIdentity{
		ServiceName: serviceName,
		OrgID:       orgID,
		IssuedAt:    now,
		ExpiresAt:   now.Add(24 * time.Hour),
	}
	fingerprint := fmt.Sprintf("%s::%s::%d", serviceName, orgID, now.UnixNano())

	a.mu.Lock()
	a.certs[fingerprint] = &certRecord{
		Identity: identity,
		CertPEM:  certPEM,
		KeyPEM:   keyPEM,
	}
	a.mu.Unlock()

	return certPEM, keyPEM, nil
}

func (a *MTLSAdapter) VerifyClientCert(ctx context.Context, certPEM []byte) (*MTLSIdentity, error) {
	if len(certPEM) == 0 {
		return nil, fmt.Errorf("mtls: certPEM is required")
	}

	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, fmt.Errorf("mtls: invalid PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("mtls: cert parse failed: %w", err)
	}

	// CA 서명 검증.
	pool := x509.NewCertPool()
	pool.AddCert(a.caCert)
	opts := x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if _, err := cert.Verify(opts); err != nil {
		return nil, fmt.Errorf("mtls: cert verification failed: %w", err)
	}

	// 만료 검사.
	if time.Now().After(cert.NotAfter) {
		return nil, fmt.Errorf("mtls: cert expired at %v", cert.NotAfter)
	}

	// 폐기 여부 검사.
	a.mu.RLock()
	for _, rec := range a.certs {
		if string(rec.CertPEM) == string(certPEM) && rec.Revoked {
			a.mu.RUnlock()
			return nil, fmt.Errorf("mtls: cert has been revoked")
		}
	}
	a.mu.RUnlock()

	orgID := ""
	if len(cert.Subject.Organization) > 0 {
		orgID = cert.Subject.Organization[0]
	}
	return &MTLSIdentity{
		ServiceName: cert.Subject.CommonName,
		OrgID:       orgID,
		IssuedAt:    cert.NotBefore,
		ExpiresAt:   cert.NotAfter,
	}, nil
}

func (a *MTLSAdapter) RevokeServiceCert(ctx context.Context, certPEM []byte) error {
	if len(certPEM) == 0 {
		return fmt.Errorf("mtls: certPEM is required")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, rec := range a.certs {
		if string(rec.CertPEM) == string(certPEM) {
			rec.Revoked = true
			return nil
		}
	}
	return fmt.Errorf("mtls: cert not found for revocation")
}
