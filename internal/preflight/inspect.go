package preflight

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"time"
)

type FieldShape struct {
	Present bool   `json:"present"`
	Type    string `json:"type"`
}

type SessionShape struct {
	ObjectValid  bool                  `json:"objectValid"`
	Fields       map[string]FieldShape `json:"fields"`
	Expires      string                `json:"expires,omitempty"`
	ExpiryStatus string                `json:"expiryStatus"`
}

var inspectedFields = []string{
	"sessionToken", "accessToken", "access_token", "expires", "email",
	"user", "user.email", "user.id", "account", "account.id", "account.account_id",
	"accountId", "account_id",
}

// InspectSession reports only a fixed set of field names and JSON types. It
// deliberately accepts an access-token-only shape without validating it as a
// usable browser session. No returned field contains an identity or credential.
func InspectSession(reader io.Reader, now time.Time) (Report, error) {
	report := Report{Operation: "inspect-session"}
	raw, err := readBounded(reader, MaxSessionBytes)
	if err != nil {
		return report, err
	}
	var payload map[string]json.RawMessage
	if err := decodeSingle(raw, &payload); err != nil || payload == nil {
		return report, fail("SESSION_INVALID")
	}
	shape := &SessionShape{ObjectValid: true, Fields: map[string]FieldShape{}, ExpiryStatus: "absent"}
	for _, path := range inspectedFields {
		value, present := fieldAt(payload, path)
		kind := "absent"
		if present {
			kind = jsonType(value)
		}
		shape.Fields[path] = FieldShape{Present: present, Type: kind}
	}
	if value, present := payload["expires"]; present {
		shape.ExpiryStatus = "invalid"
		var text string
		if json.Unmarshal(value, &text) == nil {
			if expiry, parseErr := time.Parse(time.RFC3339Nano, text); parseErr == nil {
				shape.Expires = expiry.UTC().Format(time.RFC3339Nano)
				shape.ExpiryStatus = "expired"
				if expiry.After(now) {
					shape.ExpiryStatus = "future"
				}
			}
		}
	}
	report.Code = "SESSION_SHAPE_INSPECTED"
	report.SessionShape = shape
	return report, nil
}

func fieldAt(object map[string]json.RawMessage, path string) (json.RawMessage, bool) {
	parts := strings.Split(path, ".")
	for index, part := range parts {
		value, ok := object[part]
		if !ok {
			return nil, false
		}
		if index == len(parts)-1 {
			return value, true
		}
		var child map[string]json.RawMessage
		if json.Unmarshal(value, &child) != nil || child == nil {
			return nil, false
		}
		object = child
	}
	return nil, false
}

func jsonType(value json.RawMessage) string {
	value = bytes.TrimSpace(value)
	if len(value) == 0 {
		return "invalid"
	}
	switch value[0] {
	case '{':
		return "object"
	case '[':
		return "array"
	case '"':
		return "string"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	default:
		return "number"
	}
}
