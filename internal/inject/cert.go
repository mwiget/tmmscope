package inject

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"
)

// webhookCerts holds the base64(PEM) blobs the manifest template needs: a CA
// bundle for the MutatingWebhookConfiguration and a serving cert/key for the
// webhook's TLS Secret.
type webhookCerts struct {
	CABundleB64   string
	ServerCertB64 string
	ServerKeyB64  string
}

// generateWebhookCerts mints a self-signed CA and a serving cert valid for the
// webhook Service's in-cluster DNS names. Self-signed keeps tmmscope free of any
// cert-manager dependency — it works on any cluster.
func generateWebhookCerts(service, namespace string) (*webhookCerts, error) {
	notBefore := time.Now().Add(-time.Hour)
	notAfter := time.Now().Add(10 * 365 * 24 * time.Hour) // 10y — infra plumbing

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "tmmscope-webhook-ca"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, err
	}

	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	dns := []string{
		fmt.Sprintf("%s.%s.svc", service, namespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", service, namespace),
	}
	srvTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: dns[0]},
		DNSNames:     dns,
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	srvKeyDER, err := x509.MarshalPKCS8PrivateKey(srvKey)
	if err != nil {
		return nil, err
	}

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	srvCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srvDER})
	srvKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: srvKeyDER})

	b64 := base64.StdEncoding.EncodeToString
	return &webhookCerts{
		CABundleB64:   b64(caPEM),
		ServerCertB64: b64(srvCertPEM),
		ServerKeyB64:  b64(srvKeyPEM),
	}, nil
}
