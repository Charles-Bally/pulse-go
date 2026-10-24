package pulse

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

const maxResponseBytes = 64 << 10

var fingerprintPattern = regexp.MustCompile(`^([0-9A-F]{2}:){31}[0-9A-F]{2}$`)

func normalizeFingerprint(raw string) string {
	value := strings.ToUpper(strings.TrimSpace(raw))
	if !fingerprintPattern.MatchString(value) {
		return ""
	}
	return value
}

func formatFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	parts := make([]string, len(sum))
	for index, b := range sum {
		parts[index] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

func newClient(fingerprint string) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if fingerprint != "" {
		transport.TLSClientConfig = &tls.Config{
			// Un relais sans domaine signe son propre certificat : la chaîne n'est pas vérifiée, l'empreinte du certificat l'est avant tout envoi.
			InsecureSkipVerify: true,
			VerifyConnection: func(state tls.ConnectionState) error {
				if len(state.PeerCertificates) == 0 || formatFingerprint(state.PeerCertificates[0].Raw) != fingerprint {
					return errors.New("certificat du relais inattendu : vérifie PULSE_RELAY_FINGERPRINT")
				}
				return nil
			},
		}
	}
	return &http.Client{Transport: transport, Timeout: requestTimeout}
}

func (c *config) post(ctx context.Context, path string, body any) (int, []byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+path, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("content-type", "application/json")
	request.Header.Set("x-pulse-key", c.key)
	request.Header.Set("user-agent", packageName+"/"+Version)
	response, err := c.client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	return response.StatusCode, data, err
}
