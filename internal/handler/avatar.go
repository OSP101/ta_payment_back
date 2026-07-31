package handler

import (
	"bytes"
	"image"
	"image/jpeg"
	_ "image/png"
	"io"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// Profile pictures (รูปโปรไฟล์).
//
// The browser already crops and compresses before it uploads, so the bytes that
// arrive are normally a ~50 KB square JPEG. None of that is trusted: the
// endpoint re-decodes every upload and re-encodes it itself. That is what makes
// the stored file safe — a re-encode drops EXIF (which carries GPS on a phone
// photo), drops anything hidden in a trailing chunk, and proves the file really
// is an image rather than a script wearing a .jpg name. It also puts a hard
// ceiling on the stored size no client can talk its way past.

const (
	// Accepted from the wire. Generous next to what the cropper produces, so a
	// direct API caller or an odd browser is not turned away for no reason, but
	// small enough that buffering one in memory is nothing.
	avatarMaxUploadBytes = 8 * 1024 * 1024
	// Stored edge length. 512 is twice the largest place the picture is drawn
	// (the profile page, at 96px CSS ≈ 192 device px on a 2× screen), so it
	// stays sharp on retina without paying for pixels nobody sees.
	avatarStoredEdge = 512
	// Quality 82 is the knee of the curve for photos this size: ~30–60 KB, with
	// artefacts you have to go looking for.
	avatarJPEGQuality = 82
	// Decompression-bomb guard. A 60 KB PNG can declare 30000×30000 and cost
	// 3.6 GB to decode, so the dimensions are checked from the header BEFORE
	// any pixels are allocated.
	avatarMaxSourcePixels = 40 * 1000 * 1000
)

// UploadAvatar validates, normalises, and stores the caller's own picture.
// Replaces whatever was there before — this is the "add" and the "edit" both,
// since a profile picture is singular by nature.
func (h *UserHandler) UploadAvatar(c *fiber.Ctx) error {
	fh, err := c.FormFile("file")
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "กรุณาเลือกไฟล์รูปภาพ")
	}
	if fh.Size > avatarMaxUploadBytes {
		return fiber.NewError(fiber.StatusRequestEntityTooLarge, "ไฟล์ใหญ่เกิน 8MB")
	}
	src, err := fh.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	raw, err := io.ReadAll(io.LimitReader(src, avatarMaxUploadBytes+1))
	if err != nil {
		return err
	}
	if len(raw) > avatarMaxUploadBytes {
		return fiber.NewError(fiber.StatusRequestEntityTooLarge, "ไฟล์ใหญ่เกิน 8MB")
	}

	jpg, err := normalizeAvatar(raw)
	if err != nil {
		return err
	}

	uid := UserID(c)
	key, _, err := h.Svc.Storage.Save("avatars", uuid.NewString()+".jpg", bytes.NewReader(jpg))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	url, replaced, err := h.Svc.Users.SetAvatar(c.Context(), uid, key)
	if err != nil {
		// The row never took the new key, so the blob just written is an
		// orphan. Remove it rather than leave litter in the store.
		_ = h.Svc.Storage.Delete(key)
		return err
	}
	// Best-effort: a leftover old file wastes disk but breaks nothing, and the
	// user has already got what they asked for.
	if replaced != "" {
		_ = h.Svc.Storage.Delete(replaced)
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"avatar_url": url,
		"size":       len(jpg),
	})
}

// DeleteAvatar drops the caller's picture and falls the UI back to initials.
func (h *UserHandler) DeleteAvatar(c *fiber.Ctx) error {
	old, err := h.Svc.Users.ClearAvatar(c.Context(), UserID(c))
	if err != nil {
		return err
	}
	if old != "" {
		_ = h.Svc.Storage.Delete(old)
	}
	return c.JSON(fiber.Map{"ok": true})
}

// ServeAvatar streams a user's picture behind the same auth wall as the rest of
// the API. Readable by any signed-in user: names and faces already appear side
// by side in the course rosters and review queues, and the alternative — a
// per-viewer permission check on an <img> tag — buys nothing.
func (h *UserHandler) ServeAvatar(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	key, err := h.Svc.Users.AvatarKey(c.Context(), id)
	if err != nil {
		return err
	}
	if key == "" {
		return fiber.NewError(fiber.StatusNotFound, "ไม่มีรูปโปรไฟล์")
	}
	// Keys are minted by this handler alone, but a traversal check costs
	// nothing and keeps that assumption from silently becoming untrue.
	if strings.Contains(key, "..") || !strings.HasPrefix(key, "avatars/") {
		return fiber.NewError(fiber.StatusNotFound, "ไม่มีรูปโปรไฟล์")
	}
	rc, err := h.Svc.Storage.Open(key)
	if err != nil {
		return fiber.NewError(fiber.StatusNotFound, "ไม่พบรูปภาพ")
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	c.Set("Content-Type", "image/jpeg")
	// Everything stored here is a JPEG this handler produced, so the type is
	// not a guess. The URL carries ?v=<updated_at>, which is what lets the
	// cache be long: a new picture is a new URL.
	c.Set("Cache-Control", "private, max-age=86400")
	c.Set("X-Content-Type-Options", "nosniff")
	return c.Send(body)
}

// normalizeAvatar turns arbitrary uploaded bytes into a square-ish JPEG no
// larger than avatarStoredEdge on its long side. Returns a 4xx fiber error for
// anything it cannot make sense of, so callers can return it as-is.
func normalizeAvatar(raw []byte) ([]byte, error) {
	// Header first: this is the bomb guard, and it allocates nothing.
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, fiber.NewError(fiber.StatusUnsupportedMediaType, "รองรับเฉพาะไฟล์ JPEG / PNG / WebP")
	}
	switch format {
	case "jpeg", "png", "webp":
	default:
		return nil, fiber.NewError(fiber.StatusUnsupportedMediaType, "รองรับเฉพาะไฟล์ JPEG / PNG / WebP")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, fiber.NewError(fiber.StatusUnsupportedMediaType, "ไฟล์รูปภาพเสียหาย")
	}
	if cfg.Width*cfg.Height > avatarMaxSourcePixels {
		return nil, fiber.NewError(fiber.StatusRequestEntityTooLarge, "ขนาดภาพใหญ่เกินไป")
	}

	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fiber.NewError(fiber.StatusUnsupportedMediaType, "ไฟล์รูปภาพเสียหาย")
	}

	b := img.Bounds()
	w, hgt := b.Dx(), b.Dy()
	if w > avatarStoredEdge || hgt > avatarStoredEdge {
		if w >= hgt {
			hgt = hgt * avatarStoredEdge / w
			w = avatarStoredEdge
		} else {
			w = w * avatarStoredEdge / hgt
			hgt = avatarStoredEdge
		}
		if w < 1 {
			w = 1
		}
		if hgt < 1 {
			hgt = 1
		}
	}

	// Composite onto opaque white first. A transparent PNG would otherwise
	// come out of the JPEG encoder with black where the transparency was.
	dst := image.NewRGBA(image.Rect(0, 0, w, hgt))
	draw.Draw(dst, dst.Bounds(), image.White, image.Point{}, draw.Src)
	// CatmullRom is the slow, good scaler. At 512px on a picture uploaded once
	// in a while, "slow" is a fraction of a millisecond.
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Over, nil)

	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: avatarJPEGQuality}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
