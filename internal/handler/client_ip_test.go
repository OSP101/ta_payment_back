package handler

import (
	"encoding/json"
	"net"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/valyala/fasthttp"
	"ta-payment-back/internal/audit"
)

// Real socket peers are supplied to fasthttp; a forged HTTP RemoteAddr header
// would not exercise Fiber's trusted-proxy decision.
func TestClientIPAndAuditRespectTheActualProxyPeer(t *testing.T) {
	for _, tc := range []struct {
		name, peer, forwarded, want string
		forwarding bool
	}{
		{"trusted frontend", "172.28.0.10", "192.0.2.42", "192.0.2.42", true},
		{"untrusted peer cannot spoof", "198.51.100.10", "192.0.2.42", "198.51.100.10", true},
		{"missing header", "172.28.0.10", "", "172.28.0.10", true},
		{"invalid header", "172.28.0.10", "not-an-ip", "172.28.0.10", true},
		{"IPv6 client", "172.28.0.10", "2001:db8::42", "2001:db8::42", true},
		{"forwarding disabled", "172.28.0.10", "192.0.2.42", "172.28.0.10", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := fiber.Config{
				EnableTrustedProxyCheck: true,
				TrustedProxies: []string{"172.28.0.0/24"},
				EnableIPValidation: true,
			}
			if tc.forwarding { config.ProxyHeader = fiber.HeaderXForwardedFor }
			app := fiber.New(config)
			app.Use(AuditContext())
			app.Get("/who", func(c *fiber.Ctx) error {
				info := c.Locals(audit.CtxKeyRequestInfo).(audit.RequestInfo)
				return c.JSON(fiber.Map{"ip": c.IP(), "audit_ip": info.IP, "peer": c.Context().RemoteIP().String()})
			})
			var request fasthttp.Request
			request.Header.SetMethod("GET")
			request.SetRequestURI("http://example.test/who")
			request.Header.Set(fiber.HeaderXForwardedFor, tc.forwarded)
			var ctx fasthttp.RequestCtx
			ctx.Init(&request, &net.TCPAddr{IP: net.ParseIP(tc.peer), Port: 12345}, nil)
			app.Handler()(&ctx)
			if ctx.Response.StatusCode() != 200 { t.Fatalf("status=%d", ctx.Response.StatusCode()) }
			var result map[string]string
			if err := json.Unmarshal(ctx.Response.Body(), &result); err != nil { t.Fatal(err) }
			if result["ip"] != tc.want || result["audit_ip"] != tc.want || result["peer"] != tc.peer {
				t.Fatalf("client/audit/peer mismatch: %v", result)
			}
		})
	}
}
