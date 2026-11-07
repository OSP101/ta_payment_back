package regkku

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const SourceNew = "new"
const SourceLegacy = "legacy"

// Availability describes access from the TA backend, not from the user's PC.
type Availability struct {
	Source    string     `json:"source"`
	Available bool       `json:"available"`
	State     string     `json:"state"`
	Message   string     `json:"message"`
	Action    string     `json:"action"`
	CheckedAt time.Time  `json:"checked_at"`
	RetryAt   *time.Time `json:"retry_at,omitempty"`
}

// AccessError retains a server's actual refusal and retry deadline. Response
// bodies and credentials never become user-facing errors.
type AccessError struct {
	Status    int
	RetryAt   time.Time
	Challenge bool
}

func (e *AccessError) Error() string { return fmt.Sprintf("regkku: HTTP %d", e.Status) }
func (e *AccessError) Is(target error) bool {
	return target == ErrBusy && (e.Status == 429 || e.Status == 503)
}

func retryDelay(h http.Header) time.Duration {
	if at, err := http.ParseTime(h.Get("Retry-After")); err == nil {
		return max(0, time.Until(at))
	}
	if n, err := time.ParseDuration(h.Get("Retry-After") + "s"); err == nil && n >= 0 {
		return n
	}
	return 30 * time.Second
}

func DescribeAccess(source string, err error) Availability {
	a := Availability{Source: source, CheckedAt: time.Now(), State: "available", Available: err == nil, Message: "พร้อมดึงข้อมูล", Action: "เลือกเว็บไซต์นี้เพื่อดึงข้อมูลได้"}
	if err == nil {
		return a
	}
	a.State, a.Message, a.Action = "unavailable", "เว็บไซต์ไม่พร้อมใช้งาน ดึงจากแหล่งนี้ไม่ได้", "ลองตรวจสอบอีกครั้งภายหลัง หรือเลือก REG อีกระบบ หรือวางข้อมูลจาก Excel"
	var access *AccessError
	var network net.Error
	switch {
	case errors.As(err, &access):
		if !access.RetryAt.IsZero() {
			at := access.RetryAt
			a.RetryAt = &at
		}
		switch {
		case access.Challenge || access.Status == 401 || access.Status == 403:
			a.State, a.Message, a.Action = "blocked", "เว็บไซต์ปฏิเสธการเข้าถึงหรือให้ยืนยันตัวตน ดึงจากแหล่งนี้ไม่ได้", "อย่าลองซ้ำถี่ ๆ ให้ผู้ดูแลตรวจเครือข่ายหรือสิทธิ์ของเครื่องเซิร์ฟเวอร์ และติดต่อสำนักทะเบียนหากยังเข้าไม่ได้"
		case access.Status == 429:
			a.State, a.Message, a.Action = "limited", "REG จำกัดจำนวนคำขอชั่วคราว ดึงจากแหล่งนี้ไม่ได้", "รอจนถึงเวลาที่ระบุแล้วกดตรวจสอบอีกครั้ง ไม่ต้องกดดึงซ้ำระหว่างรอ"
		case access.Status == 503:
			a.State, a.Message, a.Action = "unavailable", "REG ไม่พร้อมให้บริการชั่วคราว ดึงจากแหล่งนี้ไม่ได้", "รอจนถึงเวลาที่ระบุแล้วกดตรวจสอบอีกครั้ง หรือเลือก REG อีกระบบ"
		}
	case errors.Is(err, ErrBusy):
		a.State, a.Message, a.Action = "limited", "REG ขอให้พักการเรียกข้อมูลชั่วคราว", "รอสักครู่แล้วกดตรวจสอบอีกครั้ง ไม่ต้องกดดึงซ้ำระหว่างรอ"
	case errors.Is(err, context.DeadlineExceeded) || errors.As(err, &network) && network.Timeout():
		a.State, a.Message, a.Action = "timeout", "เชื่อมต่อ REG ไม่ทันเวลา ดึงจากแหล่งนี้ไม่ได้", "ลองตรวจสอบอีกครั้งภายหลัง หากยังไม่ได้ให้ผู้ดูแลตรวจ DNS และการเชื่อมต่อจากเครื่องเซิร์ฟเวอร์"
	case errors.Is(err, ErrLayout):
		a.State, a.Message, a.Action = "invalid_response", "REG ตอบข้อมูลที่ระบบยังอ่านไม่ได้", "เลือก REG อีกระบบหรือวางข้อมูลจาก Excel และแจ้งผู้ดูแลตรวจรูปแบบข้อมูลก่อนลองใหม่"
	}
	return a
}

func lockContext(ctx context.Context, mu *sync.Mutex) error {
	for !mu.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	return nil
}

func challengePage(body string) bool {
	s := strings.ToLower(body)
	return strings.Contains(s, "captcha") || strings.Contains(s, "just a moment") || strings.Contains(s, "access denied") || strings.Contains(s, "verify you are human")
}

func (c *Client) Probe(ctx context.Context) error {
	if err := lockContext(ctx, &c.mu); err != nil {
		return err
	}
	defer c.mu.Unlock()
	if time.Now().Before(c.cooldownUntil) {
		return c.lastAccess
	}
	body, _, err := c.once(ctx, "class_info.asp", url.Values{})
	if err != nil {
		return err
	}
	if !strings.Contains(strings.ToLower(body), "coursecode") || !strings.Contains(strings.ToLower(body), "class_info_1.asp") {
		if challengePage(body) {
			c.lastAccess = &AccessError{Status: 403, Challenge: true, RetryAt: time.Now().Add(time.Minute)}
			c.cooldownUntil = c.lastAccess.RetryAt
			return c.lastAccess
		}
		return ErrLayout
	}
	return nil
}

func (c *APIClient) Probe(ctx context.Context) error {
	if err := lockContext(ctx, &c.mu); err != nil {
		return err
	}
	defer c.mu.Unlock()
	if time.Now().Before(c.cooldownUntil) {
		if c.lastAccess != nil {
			return c.lastAccess
		}
		return ErrBusy
	}
	c.probing = true
	defer func() { c.probing = false }()
	var levels []apiLevel
	if err := c.requestLocked(ctx, "ComboLev/AllClassinfo", false, &levels); err != nil {
		return err
	}
	if len(levels) == 0 {
		return ErrLayout
	}
	for _, l := range levels {
		if l.ID <= 0 || l.Name == "" {
			return ErrLayout
		}
	}
	c.levels, c.levelsUntil = levels, time.Now().Add(time.Hour)
	return nil
}
