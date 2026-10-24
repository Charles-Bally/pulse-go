package pulse

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const maxErrorChars = 300

var (
	credentialsInURL = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^\s/@]*@`)
	secretAssignment = regexp.MustCompile(`(?i)(password|passwd|pwd|secret|token|key)=\S+`)
	authorization    = regexp.MustCompile(`\b(Bearer|Basic)\s+[A-Za-z0-9._~+/=-]{8,}`)
	ingestKey        = regexp.MustCompile(`\bpk_(live|test|agent)_[A-Za-z0-9_-]+`)
	urlQuery         = regexp.MustCompile(`(?i)(https?://[^\s?#]+)[?#]\S*`)
)

// Identifiants d'URL, jetons et mots de passe ne partent jamais, même au détour d'un message.
func redactSecrets(text string) string {
	text = credentialsInURL.ReplaceAllString(text, "${1}***@")
	text = secretAssignment.ReplaceAllString(text, "${1}=***")
	text = authorization.ReplaceAllString(text, "${1} ***")
	return ingestKey.ReplaceAllString(text, "pk_${1}_***")
}

func sanitizeError(err error) string {
	if err == nil {
		return "erreur inconnue"
	}
	var request *url.Error
	if errors.As(err, &request) && request.Err != nil {
		err = request.Err
	}
	code := ""
	var coded interface{ Code() string }
	if errors.As(err, &coded) {
		code = coded.Code()
	}
	cleaned := strings.TrimSpace(urlQuery.ReplaceAllString(redactSecrets(err.Error()), "${1}"))
	if code != "" && !strings.Contains(cleaned, code) {
		cleaned = strings.TrimSpace(code + " " + cleaned)
	}
	if cleaned == "" {
		cleaned = "erreur inconnue"
	}
	return truncate(cleaned, maxErrorChars)
}

// Les longueurs sont comptées comme l'API les compte : en unités UTF-16.
func truncate(text string, limit int) string {
	units := 0
	for index, char := range text {
		width := 1
		if char > 0xFFFF {
			width = 2
		}
		if units+width > limit {
			return text[:index]
		}
		units += width
	}
	return text
}

func isoTime(at time.Time) string {
	return at.UTC().Format("2006-01-02T15:04:05.000Z")
}
