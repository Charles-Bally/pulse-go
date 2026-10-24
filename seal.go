package pulse

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
)

const keyIDHexLength = 16

var (
	publicKeyPattern = regexp.MustCompile(`^pulse_pub_([A-Za-z0-9_-]{43})$`)
	sealInfo         = []byte("pulse/v1")
)

type sealedEnvelope struct {
	V   int    `json:"v"`
	Kid string `json:"kid"`
	Enc string `json:"enc"`
	Ct  string `json:"ct"`
}

func parsePublicKey(text string) []byte {
	match := publicKeyPattern.FindStringSubmatch(strings.TrimSpace(text))
	if match == nil {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(match[1])
	if err != nil || len(raw) != 32 {
		return nil
	}
	return raw
}

func keyID(publicKey []byte) string {
	sum := sha256.Sum256(publicKey)
	return hex.EncodeToString(sum[:])[:keyIDHexLength]
}

// L'usage entre dans les données authentifiées : un rapport scellé ne peut pas passer pour des journaux.
func sealAAD(purpose, kid string) []byte { return []byte("pulse/" + purpose + "/" + kid) }

func sealJSON(publicKey []byte, purpose string, value any) (sealedEnvelope, error) {
	plaintext, err := json.Marshal(value)
	if err != nil {
		return sealedEnvelope{}, err
	}
	kid := keyID(publicKey)
	enc, ct, err := hpkeSeal(publicKey, sealInfo, sealAAD(purpose, kid), plaintext, aes256GCM, nil)
	if err != nil {
		return sealedEnvelope{}, err
	}
	return sealedEnvelope{
		V:   1,
		Kid: kid,
		Enc: base64.RawURLEncoding.EncodeToString(enc),
		Ct:  base64.RawURLEncoding.EncodeToString(ct),
	}, nil
}

func openJSON(privateKey, publicKey []byte, purpose string, envelope sealedEnvelope, out any) error {
	enc, err := base64.RawURLEncoding.DecodeString(envelope.Enc)
	if err != nil {
		return err
	}
	ct, err := base64.RawURLEncoding.DecodeString(envelope.Ct)
	if err != nil {
		return err
	}
	plaintext, err := hpkeOpen(privateKey, sealInfo, sealAAD(purpose, keyID(publicKey)), enc, ct, aes256GCM)
	if err != nil {
		return err
	}
	return json.Unmarshal(plaintext, out)
}

func problemCount(rep report) int {
	problems := 0
	for _, check := range rep.Checks {
		if !check.OK {
			problems++
		}
	}
	for _, cron := range rep.Crons {
		if cron.LastRunOK != nil && !*cron.LastRunOK {
			problems++
		}
	}
	return problems
}

func clearPart(rep report, fields []string) map[string]any {
	visible := map[string]any{}
	if slices.Contains(fields, "status") {
		problems := problemCount(rep)
		visible["status"] = map[string]any{"ok": problems == 0, "problems": problems}
	}
	if slices.Contains(fields, "checks") {
		visible["checks"] = rep.Checks
	}
	if slices.Contains(fields, "crons") {
		visible["crons"] = rep.Crons
	}
	if slices.Contains(fields, "versions") {
		visible["packageName"] = rep.PackageName
		visible["packageVersion"] = rep.PackageVersion
		visible["framework"] = rep.Framework
		for key, value := range map[string]string{
			"frameworkVersion": rep.FrameworkVersion,
			"appVersion":       rep.AppVersion,
			"deployedAt":       rep.DeployedAt,
		} {
			if value != "" {
				visible[key] = value
			}
		}
	}
	return visible
}
