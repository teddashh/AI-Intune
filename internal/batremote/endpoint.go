package batremote

import (
	"bytes"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	LoopbackHost        = "127.0.0.1"
	CertificateFileName = "server-cert.enc.json"

	maxEndpointFileSize = 1 << 20
	maxEnvelopeDepth    = 8
)

var (
	ErrCertificateRead    = errors.New("batremote: cannot read certificate file")
	ErrCertificateMissing = errors.New("batremote: certificate envelope contains no readable certificate")
	ErrEnvelopeMalformed  = errors.New("batremote: certificate envelope is not a JSON object")
	ErrEnvelopeDepth      = errors.New("batremote: certificate envelope exceeds nesting limit")
	ErrEndpointFileSize   = errors.New("batremote: endpoint file exceeds size limit")
	ErrTokenRead          = errors.New("batremote: cannot read token file")
	ErrTokenEmpty         = errors.New("batremote: token file is empty")
	ErrEndpointPort       = errors.New("batremote: invalid endpoint port")
)

// CertificateFingerprintFromEnvelope hashes the first CERTIFICATE block in
// JSON value order, including strings containing nested JSON. Only the
// fingerprint leaves this function; other PEM blocks are discarded immediately.
// The root must be an object. Both JSON nesting and nested JSON strings count
// toward the depth limit, with the root at depth zero.
func CertificateFingerprintFromEnvelope(envelope []byte) (string, error) {
	if len(envelope) > maxEndpointFileSize {
		return "", ErrEndpointFileSize
	}
	envelope = bytes.TrimSpace(envelope)
	if len(envelope) == 0 || envelope[0] != '{' || !json.Valid(envelope) {
		return "", ErrEnvelopeMalformed
	}
	var fingerprint string
	var visit func(*json.Decoder, int) error
	visit = func(dec *json.Decoder, depth int) error {
		if depth > maxEnvelopeDepth {
			return ErrEnvelopeDepth
		}
		dec.UseNumber()
		value, err := dec.Token()
		if err != nil {
			return ErrEnvelopeMalformed
		}
		switch value := value.(type) {
		case json.Delim:
			for dec.More() {
				if value == '{' {
					// Object keys are not values and must not be searched.
					if _, err := dec.Token(); err != nil {
						return ErrEnvelopeMalformed
					}
				}
				if err := visit(dec, depth+1); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil {
				return ErrEnvelopeMalformed
			}
		case string:
			if json.Valid([]byte(value)) {
				return visit(json.NewDecoder(strings.NewReader(value)), depth+1)
			}
			if fingerprint != "" {
				return nil
			}
			for rest := []byte(value); len(rest) > 0; {
				block, tail := pem.Decode(rest)
				if block == nil {
					break
				}
				rest = tail
				if block.Type != "CERTIFICATE" {
					continue
				}
				fingerprint = certificateFingerprint(block.Bytes)
				break
			}
		}
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(envelope))
	if err := visit(dec, 0); err != nil {
		return "", err
	}
	if fingerprint == "" {
		return "", ErrCertificateMissing
	}
	return fingerprint, nil
}

// LoadEndpoint reads the agent's token and bat-server's certificate envelope.
// Token is returned only as part of a complete, successful Endpoint. Failures
// contain fixed categories, never file contents, paths, or underlying errors.
func LoadEndpoint(tokenPath, dataDir string, port int) (Endpoint, error) {
	if port < 1 || port > 65535 {
		return Endpoint{}, ErrEndpointPort
	}
	tokenBytes, err := readEndpointFile(tokenPath, ErrTokenRead)
	if err != nil {
		return Endpoint{}, err
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		return Endpoint{}, ErrTokenEmpty
	}
	envelope, err := readEndpointFile(filepath.Join(dataDir, CertificateFileName), ErrCertificateRead)
	if err != nil {
		return Endpoint{}, err
	}
	fingerprint, err := CertificateFingerprintFromEnvelope(envelope)
	if err != nil {
		return Endpoint{}, err
	}
	return Endpoint{Host: LoopbackHost, Port: port, Token: token, Fingerprint: fingerprint}, nil
}

func readEndpointFile(path string, kind error) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, kind
	}
	defer f.Close()
	data, err := readEndpointData(f)
	if err != nil {
		if errors.Is(err, ErrEndpointFileSize) {
			return nil, errors.Join(kind, ErrEndpointFileSize)
		}
		return nil, kind
	}
	return data, nil
}

func readEndpointData(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxEndpointFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxEndpointFileSize {
		return nil, ErrEndpointFileSize
	}
	return data, nil
}
