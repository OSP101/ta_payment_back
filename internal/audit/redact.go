package audit

import (
	"bytes"
	"encoding/json"
	"strings"
)

// marshalAuditValue is a final credential boundary for every caller, including
// nested structs and json.RawMessage. UseNumber preserves financial precision.
// Callers must still use allowlisted audit DTOs; this cannot redact free text.
func marshalAuditValue(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var decoded any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	redactCredentials(decoded)
	return json.Marshal(decoded)
}

func redactCredentials(value any) {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			if credentialField(key) {
				v[key] = "[redacted]"
			} else {
				redactCredentials(child)
			}
		}
	case []any:
		for _, child := range v {
			redactCredentials(child)
		}
	}
}

func credentialField(key string) bool {
	normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
	switch normalized {
	case "password", "passwordhash", "temppassword", "currentpassword", "newpassword",
		"token", "accesstoken", "refreshtoken", "authorization", "cookie", "setcookie",
		"secret", "clientsecret", "jwtsecret", "privatekey", "apikey",
		"totpsecret", "totpsecretenc", "totppendingsecretenc", "totpenckey", "piienckey", "tadocsenckey",
		"recoverycodes", "recoverycodehash":
		return true
	}
	return strings.HasSuffix(normalized, "password") || strings.HasSuffix(normalized, "enckey")
}
