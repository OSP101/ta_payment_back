package config

import (
	"encoding/base64"
	"bytes"
	"strings"
	"testing"
)

func TestProductionRejectsWeakKeysAndInsecureOrigins(t *testing.T) {
	for _, tc := range []struct{name,key,value string}{
		{"short signing key","JWT_SECRET","weak-test-key"},
		{"missing document encryption","TA_DOCS_ENC_KEY",""},
		{"HTTP app origin","APP_BASE_URL","http://staging.invalid"},
		{"credentialed wildcard CORS","CORS_ORIGINS","*"},
		{"missing scanner","CLAMAV_ADDR",""},
	} {
		t.Run(tc.name,func(t *testing.T){
			t.Setenv("APP_ENV","production")
			t.Setenv("JWT_SECRET",strings.Repeat("synthetic-signing-",3))
			for i,key:=range []string{"PII_ENC_KEY","TOTP_ENC_KEY","TA_DOCS_ENC_KEY"} { t.Setenv(key,base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{byte(i+1)},32))) }
			t.Setenv("APP_BASE_URL","https://staging.invalid")
			t.Setenv("CORS_ORIGINS","https://staging.invalid")
			t.Setenv("CLAMAV_ADDR","scanner.invalid:3310")
			t.Setenv("KKU_SSO_ENV","")
			if _,err:=Load();err!=nil { t.Fatal("valid synthetic production config rejected",err) }
			t.Setenv(tc.key,tc.value)
			if _,err:=Load();err==nil { t.Fatal("unsafe production config accepted") }
		})
	}
}
