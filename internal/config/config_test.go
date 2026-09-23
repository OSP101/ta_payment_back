package config

import (
	"testing"

	"ta-payment-back/internal/ssonext"
)

func TestSSOEnvPicksHostPair(t *testing.T) {
	cases := []struct {
		name               string
		env                map[string]string
		wantLogin, wantAPI string
		wantErr            bool
	}{
		// Empty bases mean ssonext's own production defaults.
		{name: "default is prod", env: map[string]string{}, wantLogin: "", wantAPI: ""},
		{name: "prod", env: map[string]string{"KKU_SSO_ENV": "prod"}, wantLogin: "", wantAPI: ""},
		{name: "uat", env: map[string]string{"KKU_SSO_ENV": "uat"},
			wantLogin: ssonext.UATLoginBase, wantAPI: ssonext.UATAPIBase},
		{name: "case and spaces tolerated", env: map[string]string{"KKU_SSO_ENV": " UAT "},
			wantLogin: ssonext.UATLoginBase, wantAPI: ssonext.UATAPIBase},
		{name: "explicit base beats uat", env: map[string]string{
			"KKU_SSO_ENV": "uat", "KKU_SSO_WEB_BASE_URL": "http://localhost:8099", "KKU_SSO_API_BASE_URL": "http://localhost:8099"},
			wantLogin: "http://localhost:8099", wantAPI: "http://localhost:8099"},
		{name: "typo refuses to start", env: map[string]string{"KKU_SSO_ENV": "production"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("JWT_SECRET", "test")
			t.Setenv("PII_ENC_KEY", "test")
			t.Setenv("TOTP_ENC_KEY", "test")
			for _, k := range []string{"KKU_SSO_ENV", "KKU_SSO_WEB_BASE_URL", "KKU_SSO_API_BASE_URL"} {
				t.Setenv(k, tc.env[k])
			}
			c, err := Load()
			if tc.wantErr {
				if err == nil {
					t.Fatal("Load() succeeded, want a KKU_SSO_ENV error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load(): %v", err)
			}
			if c.SSOLoginBase != tc.wantLogin || c.SSOAPIBase != tc.wantAPI {
				t.Errorf("bases = %q, %q; want %q, %q", c.SSOLoginBase, c.SSOAPIBase, tc.wantLogin, tc.wantAPI)
			}
		})
	}
}

// The two URLs filed with สำนักฯ default to this app's own pages, and single
// logout stays OFF unless a deployment asks for it — SSONext's logout ends
// the KKU session globally, which is not what clicking "ออกจากระบบ" here
// should mean.
func TestSSOCallbackDefaultsAndSingleLogout(t *testing.T) {
	base := func(t *testing.T) Config {
		t.Helper()
		t.Setenv("JWT_SECRET", "test")
		t.Setenv("PII_ENC_KEY", "test")
		t.Setenv("TOTP_ENC_KEY", "test")
		// A list, as a real deployment has it: the public origin plus a LAN
		// address. Only the first is a URL KKU could ever redirect to.
		t.Setenv("APP_BASE_URL", "https://coco.example.ac.th/,http://10.199.10.10:3000")
		c, err := Load()
		if err != nil {
			t.Fatalf("Load(): %v", err)
		}
		return c
	}

	c := base(t)
	if c.SSORedirect != "https://coco.example.ac.th/login/sso" {
		t.Errorf("SSORedirect = %q", c.SSORedirect)
	}
	if c.SSOLogoutRedirect != "https://coco.example.ac.th/login" {
		t.Errorf("SSOLogoutRedirect = %q", c.SSOLogoutRedirect)
	}
	if c.SSOSingleLogout {
		t.Error("single logout must default to off")
	}

	t.Run("explicit values win", func(t *testing.T) {
		t.Setenv("KKU_SSO_REDIRECT_URL", "https://cocolabs.example.ac.th/api/auth/kku/callback")
		t.Setenv("KKU_SSO_LOGOUT_REDIRECT_URL", "https://cocolabs.example.ac.th/logout")
		t.Setenv("KKU_SSO_SINGLE_LOGOUT", "true")
		c := base(t)
		if c.SSORedirect != "https://cocolabs.example.ac.th/api/auth/kku/callback" ||
			c.SSOLogoutRedirect != "https://cocolabs.example.ac.th/logout" || !c.SSOSingleLogout {
			t.Errorf("config = %q, %q, %v", c.SSORedirect, c.SSOLogoutRedirect, c.SSOSingleLogout)
		}
	})
}

// สำนักฯ issues some apps one value for both fields (the cocolabs
// registration is one), so an unset App ID means "same as Client ID".
func TestSSOAppIDFallsBackToClientID(t *testing.T) {
	t.Setenv("JWT_SECRET", "test")
	t.Setenv("PII_ENC_KEY", "test")
	t.Setenv("TOTP_ENC_KEY", "test")
	t.Setenv("KKU_SSO_CLIENT_ID", "01a055d5-ffff")
	t.Setenv("KKU_SSO_CLIENT_SECRET", "secret")

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.SSOAppID != "01a055d5-ffff" || !c.SSOEnabled {
		t.Fatalf("AppID = %q, enabled = %v", c.SSOAppID, c.SSOEnabled)
	}

	t.Run("an explicit App ID is left alone", func(t *testing.T) {
		t.Setenv("KKU_SSO_APP_ID", "different-app")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.SSOAppID != "different-app" {
			t.Errorf("AppID = %q", c.SSOAppID)
		}
	})
}
