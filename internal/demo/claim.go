package demo

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
)

// A slot claim credential binds a slot's login to the browser that CLAIMED it.
//
// Every slot's /auth/login route is reachable by anyone, and the demo password
// is public by design. Login used to decide the tier ceiling from whoever owns
// the slot — so a TA-tier tester could POST to ANOTHER tester's slot (indices
// are just 0..N-1) and log in there as staff or admin under that owner's tier.
// Now /enter hands the claiming browser a signed, HttpOnly credential naming
// the slot and the email it was claimed with, and a slot login requires it:
// the tier comes from that email, and only while that email still owns the slot.
//
// What this does NOT change: /enter itself still trusts the email typed into it
// (the sandbox has no email verification). Knowing another authorised tester's
// email therefore still lets you enter as them; this closes the path that
// needed no such knowledge — iterating slot numbers.

const (
	claimCookieName = "demo_slot_claim"
	// Scoped to the demo API: sent to /enter's siblings and every slot route,
	// never to the real /api/v1 surface.
	claimCookiePath = apiRoot
	claimTTL        = 12 * time.Hour
)

var errBadClaim = errors.New("demo: missing or invalid slot claim")

type slotClaim struct {
	Slot  int
	Email string
}

func claimMAC(key []byte, payload string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("demo-slot-claim\x00")) // domain separation from the session JWT
	m.Write([]byte(payload))
	return m.Sum(nil)
}

func signClaim(key []byte, cl slotClaim, now time.Time) string {
	payload := fmt.Sprintf("%d|%d|%s", cl.Slot, now.Add(claimTTL).Unix(), normalizeEmail(cl.Email))
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(payload)) + "." + enc.EncodeToString(claimMAC(key, payload))
}

func verifyClaim(key []byte, raw string, now time.Time) (slotClaim, error) {
	enc := base64.RawURLEncoding
	p, s, ok := strings.Cut(raw, ".")
	if !ok {
		return slotClaim{}, errBadClaim
	}
	payloadB, err1 := enc.DecodeString(p)
	sig, err2 := enc.DecodeString(s)
	if err1 != nil || err2 != nil {
		return slotClaim{}, errBadClaim
	}
	payload := string(payloadB)
	if !hmac.Equal(sig, claimMAC(key, payload)) {
		return slotClaim{}, errBadClaim
	}
	parts := strings.SplitN(payload, "|", 3)
	if len(parts) != 3 {
		return slotClaim{}, errBadClaim
	}
	slot, err1 := strconv.Atoi(parts[0])
	exp, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil || parts[2] == "" {
		return slotClaim{}, errBadClaim
	}
	if now.Unix() > exp {
		return slotClaim{}, errBadClaim
	}
	return slotClaim{Slot: slot, Email: parts[2]}, nil
}

func setClaimCookie(c *fiber.Ctx, value string, secure bool) {
	c.Cookie(&fiber.Cookie{
		Name:     claimCookieName,
		Value:    value,
		Path:     claimCookiePath,
		MaxAge:   int(claimTTL.Seconds()),
		HTTPOnly: true,
		Secure:   secure,
		SameSite: fiber.CookieSameSiteLaxMode,
	})
}
