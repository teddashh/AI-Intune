package batremote

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const endpointTokenSentinel = "SENTINEL-TOKEN-VALUE"

type endpointCertificate struct {
	certPEM, keyPEM, keyFragment, fingerprint string
}

func newEndpointCertificate(t *testing.T) endpointCertificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal("generate test key failed")
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal("generate test certificate failed")
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal("marshal test key failed")
	}
	digest := sha256.Sum256(der)
	parts := make([]string, len(digest))
	for i, b := range digest {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return endpointCertificate{
		certPEM:     string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		keyPEM:      string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})),
		keyFragment: base64.StdEncoding.EncodeToString(keyDER)[16:48],
		fingerprint: strings.Join(parts, ":"),
	}
}

func endpointJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal("marshal test envelope failed")
	}
	return string(data)
}

func endpointEnvelope(t *testing.T, data string) string {
	t.Helper()
	return endpointJSON(t, map[string]any{"enc": false, "data": data})
}

func assertEndpointFingerprint(t *testing.T, envelope, want string) {
	t.Helper()
	got, err := CertificateFingerprintFromEnvelope([]byte(envelope))
	if err != nil || got != want {
		t.Fatal("certificate fingerprint mismatch or unexpected error")
	}
}

func TestCertificateFingerprintFromPlaintextEnvelope(t *testing.T) {
	c := newEndpointCertificate(t)
	assertEndpointFingerprint(t, endpointEnvelope(t, c.certPEM+c.keyPEM), c.fingerprint)
}

func TestCertificateFingerprintWhenPrivateKeyComesFirst(t *testing.T) {
	c := newEndpointCertificate(t)
	assertEndpointFingerprint(t, endpointEnvelope(t, c.keyPEM+c.certPEM), c.fingerprint)
}

func TestCertificateFingerprintFromNestedEnvelope(t *testing.T) {
	c := newEndpointCertificate(t)
	nested := endpointJSON(t, map[string]any{"certificate": c.certPEM + c.keyPEM})
	assertEndpointFingerprint(t, endpointEnvelope(t, nested), c.fingerprint)
	array := endpointJSON(t, map[string]any{"unknown": []any{nil, true, map[string]any{"another": nested}}})
	assertEndpointFingerprint(t, array, c.fingerprint)
}

func TestCertificateFingerprintRejectsEncrypted(t *testing.T) {
	random := make([]byte, 96)
	if _, err := rand.Read(random); err != nil {
		t.Fatal("generate ciphertext fixture failed")
	}
	envelope := endpointJSON(t, map[string]any{"enc": true, "data": base64.StdEncoding.EncodeToString(random)})
	got, err := CertificateFingerprintFromEnvelope([]byte(envelope))
	if got != "" || !errors.Is(err, ErrCertificateMissing) {
		t.Fatal("encrypted envelope must fail without a fingerprint")
	}
}

func TestCertificateFingerprintRejectsMissingCertificate(t *testing.T) {
	c := newEndpointCertificate(t)
	for _, envelope := range []string{
		endpointEnvelope(t, c.keyPEM),
		endpointJSON(t, map[string]string{c.certPEM: c.keyPEM}),
		endpointEnvelope(t, "-----BEGIN CERTIFICATE-----\n!invalid!\n-----END CERTIFICATE-----"),
	} {
		got, err := CertificateFingerprintFromEnvelope([]byte(envelope))
		if got != "" || !errors.Is(err, ErrCertificateMissing) {
			t.Fatal("missing certificate must fail without a fingerprint")
		}
	}
}

func TestCertificateFingerprintUsesFirstCertificate(t *testing.T) {
	first, second := newEndpointCertificate(t), newEndpointCertificate(t)
	assertEndpointFingerprint(t, endpointEnvelope(t, first.certPEM+second.certPEM), first.fingerprint)
	// Preserve source order even when object keys are in reverse lexical order.
	object := `{"z":` + endpointJSON(t, first.certPEM) + `,"a":` + endpointJSON(t, second.certPEM) + `}`
	assertEndpointFingerprint(t, object, first.fingerprint)
	array := endpointJSON(t, map[string]any{"items": []string{first.certPEM, second.certPEM}})
	assertEndpointFingerprint(t, array, first.fingerprint)
}

func writeEndpointFixture(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal("write endpoint fixture failed")
	}
}

func TestLoadEndpointReadsTokenAndFingerprint(t *testing.T) {
	c := newEndpointCertificate(t)
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	writeEndpointFixture(t, tokenPath, " \t\n"+endpointTokenSentinel+"\r\n")
	writeEndpointFixture(t, filepath.Join(dir, CertificateFileName), endpointEnvelope(t, c.certPEM+c.keyPEM))
	ep, err := LoadEndpoint(tokenPath, dir, 12345)
	if err != nil {
		t.Fatal("load endpoint unexpectedly failed")
	}
	if ep.Host != LoopbackHost || ep.Port != 12345 || ep.Token != endpointTokenSentinel || ep.Fingerprint != c.fingerprint {
		t.Fatal("loaded endpoint fields differ from expected values")
	}
}

type endpointFailureCase struct {
	name, token, envelope                                                  string
	missingToken, missingCertificate, tokenDirectory, certificateDirectory bool
	want                                                                   error
}

func endpointFailureCases(t *testing.T, c endpointCertificate) []endpointFailureCase {
	t.Helper()
	deep := endpointJSON(t, c.keyPEM)
	for range maxEnvelopeDepth + 1 {
		deep = `{"nested":` + deep + `}`
	}
	deepString := c.keyPEM
	for range maxEnvelopeDepth + 1 {
		deepString = endpointEnvelope(t, deepString)
	}
	return []endpointFailureCase{
		{name: "empty token", token: " \t\r\n", want: ErrTokenEmpty},
		{name: "missing token", missingToken: true, want: ErrTokenRead},
		{name: "missing certificate", missingCertificate: true, want: ErrCertificateRead},
		{name: "token directory", tokenDirectory: true, want: ErrTokenRead},
		{name: "certificate directory", certificateDirectory: true, want: ErrCertificateRead},
		{name: "oversize token", token: endpointTokenSentinel + strings.Repeat("x", maxEndpointFileSize), want: ErrEndpointFileSize},
		{name: "oversize certificate", envelope: endpointEnvelope(t, c.keyPEM+strings.Repeat("x", maxEndpointFileSize)), want: ErrEndpointFileSize},
		{name: "encrypted", envelope: `{"enc":true,"data":"` + base64.StdEncoding.EncodeToString([]byte(c.keyPEM)) + `"}`, want: ErrCertificateMissing},
		{name: "only key", envelope: endpointEnvelope(t, c.keyPEM), want: ErrCertificateMissing},
		{name: "empty object", envelope: `{}`, want: ErrCertificateMissing},
		{name: "not JSON", envelope: endpointTokenSentinel + c.keyPEM, want: ErrEnvelopeMalformed},
		{name: "JSON array", envelope: endpointJSON(t, []string{c.keyPEM}), want: ErrEnvelopeMalformed},
		{name: "JSON null", envelope: `null`, want: ErrEnvelopeMalformed},
		{name: "trailing data", envelope: endpointEnvelope(t, c.certPEM+c.keyPEM) + endpointTokenSentinel, want: ErrEnvelopeMalformed},
		{name: "depth", envelope: deep, want: ErrEnvelopeDepth},
		{name: "string depth", envelope: deepString, want: ErrEnvelopeDepth},
		{name: "depth after certificate", envelope: `{"first":` + endpointJSON(t, c.certPEM) + `,"deep":` + deep + `}`, want: ErrEnvelopeDepth},
	}
}

func runEndpointFailure(t *testing.T, tc endpointFailureCase, c endpointCertificate) error {
	t.Helper()
	// Sensitive path components must not be included in errors either.
	dir := filepath.Join(t.TempDir(), endpointTokenSentinel+"PRIVATE KEY")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal("create fixture directory failed")
	}
	tokenPath := filepath.Join(dir, "token")
	certPath := filepath.Join(dir, CertificateFileName)
	token := tc.token
	if token == "" {
		token = endpointTokenSentinel
	}
	envelope := tc.envelope
	if envelope == "" {
		envelope = endpointEnvelope(t, c.certPEM+c.keyPEM)
	}
	for _, file := range []struct {
		path, contents     string
		missing, directory bool
	}{
		{tokenPath, token, tc.missingToken, tc.tokenDirectory},
		{certPath, envelope, tc.missingCertificate, tc.certificateDirectory},
	} {
		if file.directory {
			if err := os.Mkdir(file.path, 0700); err != nil {
				t.Fatal("create unreadable fixture failed")
			}
		} else if !file.missing {
			writeEndpointFixture(t, file.path, file.contents)
		}
	}
	ep, err := LoadEndpoint(tokenPath, dir, 12345)
	if !errors.Is(err, tc.want) {
		t.Fatal("endpoint failure did not match expected category")
	}
	if ep != (Endpoint{}) {
		t.Fatal("failed load returned a partial endpoint")
	}
	return err
}

func TestLoadEndpointRejectsEmptyToken(t *testing.T) {
	c := newEndpointCertificate(t)
	runEndpointFailure(t, endpointFailureCases(t, c)[0], c)
}

func TestLoadEndpointRejectsMissingFiles(t *testing.T) {
	c := newEndpointCertificate(t)
	for _, tc := range endpointFailureCases(t, c) {
		if tc.missingToken || tc.missingCertificate || tc.tokenDirectory || tc.certificateDirectory {
			t.Run(tc.name, func(t *testing.T) { runEndpointFailure(t, tc, c) })
		}
	}
}

func TestLoadEndpointRejectsOversizeFiles(t *testing.T) {
	c := newEndpointCertificate(t)
	for _, tc := range endpointFailureCases(t, c) {
		if tc.want == ErrEndpointFileSize {
			t.Run(tc.name, func(t *testing.T) {
				err := runEndpointFailure(t, tc, c)
				kind := ErrCertificateRead
				if tc.token != "" {
					kind = ErrTokenRead
				}
				if !errors.Is(err, kind) {
					t.Fatal("oversize error does not identify file category")
				}
			})
		}
	}
}

func TestEndpointErrorsNeverLeakSecrets(t *testing.T) {
	c := newEndpointCertificate(t)
	check := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("expected an error")
		}
		for _, secret := range []string{endpointTokenSentinel, c.keyFragment, "PRIVATE KEY"} {
			if strings.Contains(err.Error(), secret) {
				t.Fatal("error leaked sensitive material")
			}
		}
	}
	for _, tc := range endpointFailureCases(t, c) {
		t.Run(tc.name, func(t *testing.T) {
			check(t, runEndpointFailure(t, tc, c))
			if tc.envelope != "" {
				fingerprint, err := CertificateFingerprintFromEnvelope([]byte(tc.envelope))
				check(t, err)
				if fingerprint != "" || !errors.Is(err, tc.want) {
					t.Fatal("pure parser returned a fingerprint or wrong error category")
				}
			}
		})
	}
}

func TestLoadEndpointRejectsMalformedEnvelope(t *testing.T) {
	c := newEndpointCertificate(t)
	for _, tc := range endpointFailureCases(t, c) {
		if tc.want == ErrEnvelopeMalformed || tc.want == ErrEnvelopeDepth {
			t.Run(tc.name, func(t *testing.T) { runEndpointFailure(t, tc, c) })
		}
	}
}

type endpointCountingReader struct{ read int }

func (r *endpointCountingReader) Read(p []byte) (int, error) {
	clear(p)
	r.read += len(p)
	return len(p), nil
}

func TestEndpointReadIsBounded(t *testing.T) {
	r := &endpointCountingReader{}
	data, err := readEndpointData(r)
	if !errors.Is(err, ErrEndpointFileSize) || data != nil || r.read != maxEndpointFileSize+1 {
		t.Fatal("reader exceeded byte budget or returned oversized data")
	}
	data, err = readEndpointData(io.LimitReader(&endpointCountingReader{}, maxEndpointFileSize))
	if err != nil || len(data) != maxEndpointFileSize {
		t.Fatal("file exactly at size limit was rejected")
	}
}

func TestCertificateFingerprintDepthBoundary(t *testing.T) {
	c := newEndpointCertificate(t)
	envelope := endpointJSON(t, c.certPEM)
	for range maxEnvelopeDepth {
		envelope = `{"nested":` + envelope + `}`
	}
	assertEndpointFingerprint(t, envelope, c.fingerprint)
	assertEndpointFingerprint(t, `{"number":1e99999,"data":`+endpointJSON(t, c.certPEM)+`}`, c.fingerprint)
}

func TestLoadEndpointRejectsInvalidPorts(t *testing.T) {
	for _, port := range []int{-1, 0, 65536} {
		t.Run("port", func(t *testing.T) {
			ep, err := LoadEndpoint("", "", port)
			if !errors.Is(err, ErrEndpointPort) {
				t.Fatal("expected ErrEndpointPort")
			}
			if ep != (Endpoint{}) {
				t.Fatal("failed load returned a partial endpoint")
			}
		})
	}
}
