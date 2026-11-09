package audit

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMarshalAuditValueRedactsNestedCredentialsAndPreservesMoney(t *testing.T) {
	value := json.RawMessage(`{"amount":123456789012345.67,"course_code":"CP1","items":[{"password":"test-only-password","totp_secret_enc":"test-only-secret"}],"clientSecret":"test-only-client","roles":["ta"]}`)
	raw, err := marshalAuditValue(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, credential := range []string{"test-only-password", "test-only-secret", "test-only-client"} {
		if strings.Contains(string(raw), credential) {
			t.Fatal("credential survived audit serialization")
		}
	}
	for _, preserved := range []string{`123456789012345.67`, `"course_code":"CP1"`, `"roles":["ta"]`} {
		if !strings.Contains(string(raw), preserved) {
			t.Errorf("non-credential audit data was changed: missing %s", preserved)
		}
	}
}

func TestMarshalAuditValueReturnsEncodingErrors(t *testing.T) {
	if _, err := marshalAuditValue(make(chan int)); err == nil {
		t.Fatal("unsupported audit data must fail rather than lose evidence")
	}
}

func TestMarshalAuditValueRedactsRequestStruct(t *testing.T) {
	value := struct {
		Password string `json:"password"`
		CourseID string `json:"course_id"`
	}{Password: "synthetic-credential", CourseID: "synthetic-course"}
	raw, err := marshalAuditValue(value)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), value.Password) || !strings.Contains(string(raw), value.CourseID) {
		t.Fatal("request struct credential boundary failed")
	}
}
