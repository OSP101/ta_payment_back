package demo

import (
	"errors"
	"fmt"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/limiter"

	"ta-payment-back/internal/handler"
)

// apiRoot is deliberately NOT under /api/v1 — it must share no string
// prefix with it at all. Fiber matches "Use"-registered middleware (which is
// what api.Group("", Authenticated(tokens), AccountGuard(svc)) becomes — see
// handler.MountAPI) against the REQUEST PATH by simple strings.HasPrefix,
// not by path segment. A demo root like "/api/v1/demo" — or even
// "/api/v1demo" — would still satisfy strings.HasPrefix(path, "/api/v1") and
// so would run PRODUCTION's own Authenticated(tokens)/AccountGuard(svc)
// against every demo request too, ahead of this package's own middleware,
// rejecting it with "missing token" before DemoAuthenticated ever runs
// (confirmed empirically — see the smoke test in docs/PLAN-demo-sandbox.md).
// "/api/demo" cannot collide with "/api/v1" under that same prefix check no
// matter how Fiber's matching evolves, since they diverge at the very next
// character. See next.config.ts's rewrites() for the matching frontend rule.
const apiRoot = "/api/demo"

// Mount wires the whole demo sandbox onto app: the entry/reset/status
// endpoints under apiRoot, and — per slot — a full, independently-bound copy
// of every real API route under apiRoot/w/<index>/... (via handler.MountAPI)
// plus this package's own login/logout, which SHADOW the ones
// handler.MountAPI would otherwise register (see the comment above the
// login registration below for why that shadowing is deliberate, not a bug).
//
// Like the rest of this system's route registration, this must run once at
// boot, before app.Listen — see internal/handler.MountAPI's doc comment.
// Call it after handler.Mount (production): harmless either way given the
// disjoint prefixes above, but keeps demo's slots visibly the "second half"
// of route setup in main.go.
func Mount(app *fiber.App, m *Manager) {
	root := app.Group(apiRoot)

	// Demo gets none of production's /api/v1-scoped middleware (see apiRoot's
	// doc comment on why that scoping can't be shared) — including its
	// baseline per-IP ceiling. Reinstate an equivalent one here rather than
	// leave every demo route unprotected by anything but Go's own throughput.
	root.Use(limiter.New(limiter.Config{
		Max:          600,
		Expiration:   time.Minute,
		KeyGenerator: func(c *fiber.Ctx) string { return c.IP() },
		LimitReached: func(c *fiber.Ctx) error {
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
				"error": "ทำรายการบ่อยเกินไป กรุณารอสักครู่แล้วลองใหม่",
			})
		},
	}))

	enterLimiter := limiter.New(limiter.Config{
		Max:          20,
		Expiration:   time.Minute,
		KeyGenerator: func(c *fiber.Ctx) string { return c.IP() },
		LimitReached: func(c *fiber.Ctx) error {
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
				"error": "ทำรายการบ่อยเกินไป กรุณารอสักครู่แล้วลองใหม่",
			})
		},
	})

	root.Get("/status", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"enabled": true, "max_workspaces": len(m.Slots())})
	})

	root.Post("/enter", enterLimiter, func(c *fiber.Ctx) error {
		var in struct {
			Email string `json:"email" validate:"required,email"`
		}
		if err := handler.Bind(c, &in); err != nil {
			return err
		}
		slot, tier, err := m.Claim(c.Context(), in.Email)
		if errors.Is(err, ErrNotAuthorized) {
			return fiber.NewError(fiber.StatusForbidden,
				"อีเมลนี้ยังไม่ได้รับสิทธิ์เข้าใช้งานห้องทดลอง กรุณาติดต่อเจ้าหน้าที่เพื่อขอสิทธิ์")
		}
		if errors.Is(err, ErrFull) {
			return fiber.NewError(fiber.StatusServiceUnavailable,
				"ห้องทดลองเต็มในขณะนี้ กรุณาลองใหม่อีกครั้งภายหลัง")
		}
		if err != nil {
			return err
		}
		return c.JSON(fiber.Map{
			"base_path":     slotBasePath(slot.Index),
			"accounts":      AccountsForTier(tier),
			"tier":          tier,
			"demo_password": DemoPassword,
		})
	})

	root.Post("/reset", enterLimiter, func(c *fiber.Ctx) error {
		var in struct {
			Email string `json:"email" validate:"required,email"`
		}
		if err := handler.Bind(c, &in); err != nil {
			return err
		}
		slot, err := m.Reset(c.Context(), in.Email)
		if err != nil {
			return err
		}
		return c.JSON(fiber.Map{"base_path": slotBasePath(slot.Index)})
	})

	tokens := m.Tokens()
	for _, slot := range m.Slots() {
		wsRouter := app.Group(slotBasePath(slot.Index))
		login := &LoginHandler{
			Svc: slot.Container, Tokens: tokens, BasePath: slotBasePath(slot.Index),
			Manager: m, SlotIndex: slot.Index,
		}

		// Registered BEFORE handler.MountAPI below so fiber's first-match-wins
		// route resolution picks these over the real AuthHandler.Login/Logout
		// MountAPI also registers at the same two paths — those become dead
		// routes, never reached. This is the one place this package shadows
		// rather than swaps a dependency: AuthHandler.Login always calls the
		// unexported setAuthCookie ("access_token", Path "/"), so reusing it
		// here would set that same cookie for a demo login, colliding with a
		// real production session in the same browser — see cookieName's
		// doc comment in auth.go.
		wsRouter.Post("/auth/login", login.Login)
		wsRouter.Post("/auth/logout", login.Logout)

		// See watermark_middleware.go — must be registered before MountAPI so
		// it wraps every route that mounts, present or future, without this
		// file needing to list them.
		wsRouter.Use(DemoDownloadWatermark())

		handler.MountAPI(wsRouter, slot.Container, tokens, DemoAuthenticated(tokens), slot.RBAC, slot.Auditor)

		// Simulator panel — the scenario engine (scenario.go/scenario_steps.go).
		// A small parallel authed group rather than routes bundled into
		// MountAPI: these paths (/scenario/events...) don't exist in
		// production at all, so they have no real counterpart to extend.
		scenario := wsRouter.Group("", DemoAuthenticated(tokens), handler.AccountGuard(slot.Container))
		scenario.Get("/scenario/events", func(c *fiber.Ctx) error {
			items, err := ScenarioStatus(c.Context(), slot.Container)
			if err != nil {
				return err
			}
			return c.JSON(fiber.Map{"items": items})
		})
		scenario.Post("/scenario/events/:key", func(c *fiber.Ctx) error {
			message, err := RunScenarioEvent(c.Context(), slot.Container, c.Params("key"))
			if err != nil {
				return err
			}
			return c.JSON(fiber.Map{"message": message})
		})
		scenario.Get("/scenario/problems", func(c *fiber.Ctx) error {
			return c.JSON(fiber.Map{"items": ProblemEvents()})
		})
		scenario.Post("/scenario/problems/:key", func(c *fiber.Ctx) error {
			message, err := RunProblemEvent(c.Context(), slot.Container, app, tokens, slotBasePath(slot.Index), c.Params("key"))
			if err != nil {
				return err
			}
			return c.JSON(fiber.Map{"message": message})
		})
		// "เริ่มใหม่ทั้งหมด" — wipes and reseeds THIS slot without needing the
		// claim email back (unlike /api/demo/reset): the slot is already
		// resolved by which authed route this request reached. Every
		// existing session for this slot — including the one making this
		// very request — is invalidated the moment the users table is
		// wiped and reseeded (see ResetSlot → resetSlotData), so the
		// frontend must treat any response here as "log in again", success
		// or not.
		scenario.Post("/scenario/reset", func(c *fiber.Ctx) error {
			if err := m.ResetSlot(c.Context(), slot); err != nil {
				return err
			}
			return c.JSON(fiber.Map{"ok": true})
		})

		// "บันทึกจุดตรวจสอบ" / "ย้อนกลับไปจุดตรวจสอบ" — see checkpoint.go.
		// One checkpoint per slot; saving again overwrites it.
		scenario.Get("/scenario/checkpoint", func(c *fiber.Ctx) error {
			savedAt, err := m.CheckpointSavedAt(c.Context(), slot)
			if err != nil {
				return err
			}
			return c.JSON(fiber.Map{"saved_at": savedAt})
		})
		scenario.Post("/scenario/checkpoint", func(c *fiber.Ctx) error {
			if err := m.SaveCheckpoint(c.Context(), slot); err != nil {
				return err
			}
			return c.JSON(fiber.Map{"ok": true})
		})
		// Unlike /scenario/reset, a restore does NOT touch the users table
		// (checkpoint tables are restored as-is, same rows/passwords that
		// existed when saved) — the current session stays valid, so this
		// does not need the frontend to treat every response as "log in
		// again" the way reset does.
		scenario.Post("/scenario/checkpoint/restore", func(c *fiber.Ctx) error {
			if err := m.RestoreCheckpoint(c.Context(), slot); err != nil {
				return err
			}
			return c.JSON(fiber.Map{"ok": true})
		})
	}
}

func slotBasePath(index int) string {
	return fmt.Sprintf("%s/w/%d", apiRoot, index)
}
