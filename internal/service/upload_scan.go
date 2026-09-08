package service

import (
	"bytes"
	"context"
	"errors"
	"log"

	"github.com/google/uuid"

	"ta-payment-back/internal/antivirus"
	"ta-payment-back/internal/audit"
)

// ScanUpload คือด่านสแกนไวรัสด่านเดียวของระบบ ทุกเส้นทางที่รับไฟล์จากผู้ใช้
// ต้องเรียกผ่านตรงนี้ — เดิม DocsService.scanUpload เป็นผู้เรียก ClamAV
// รายเดียว ทำให้ไฟล์แนบประกาศ (ซึ่งถูกเสิร์ฟให้คนไม่มีบัญชีผ่าน
// /public/announcements/media/*), avatar และรูปหลักฐานของ worklog edit batch
// ผ่านเข้าระบบโดยไม่เคยถูกสแกนเลย ทั้งที่ config บังคับ CLAMAV_ADDR ใน
// production ด้วยเหตุผลว่า upload ที่ไม่ถูกสแกนรับไม่ได้
//
// fail-closed เหมือนเดิม: สแกนไม่สำเร็จ = ปฏิเสธ ไม่ใช่ปล่อยผ่าน — ดูเหตุผล
// เต็มใน DocsService.scanUpload's ของเดิม (docs.go) ซึ่งยังอยู่และเรียกผ่าน
// เมธอดนี้เหมือนกัน เพื่อไม่ต้องแตะโค้ดที่ทดสอบไว้แล้วของเส้นทางเอกสาร TA
func (c *Container) ScanUpload(ctx context.Context, actor uuid.UUID, kind, filename string, body []byte) error {
	if c.AV == nil || !c.AV.Enabled() {
		return nil
	}
	err := c.AV.Scan(ctx, bytes.NewReader(body))
	if err == nil {
		return nil
	}

	var infected *antivirus.ErrInfected
	if errors.As(err, &infected) {
		if err := c.Auditor.Log(ctx, audit.Entry{
			ActorID: &actor, Action: "upload.infected", Entity: kind,
			After: map[string]any{
				"kind": kind, "filename": filename,
				"signature": infected.Signature, "size_bytes": len(body),
			},
		}); err != nil {
			return err
		}
		return &UserError{
			Status: 422,
			Msg:    "ไฟล์นี้ตรวจพบความเสี่ยงด้านความปลอดภัย จึงไม่รับอัปโหลด กรุณาสแกนไวรัสในเครื่องแล้วสร้างไฟล์ใหม่",
		}
	}

	// Scan could not complete. Log the reason for whoever has to fix clamd, and
	// refuse — an unscanned upload is worse than a rejected one.
	log.Printf("antivirus: scan failed for %s upload by %s: %v", kind, actor, err)
	if err := c.Auditor.Log(ctx, audit.Entry{
		ActorID: &actor, Action: "upload.scan_failed", Entity: kind,
		After: map[string]any{"kind": kind, "filename": filename, "error": err.Error()},
	}); err != nil {
		return err
	}
	return &UserError{
		Status: 503,
		Msg:    "ระบบตรวจไวรัสไม่พร้อมใช้งานชั่วคราว จึงยังไม่รับอัปโหลด กรุณาลองใหม่อีกครั้ง หากยังไม่ได้โปรดแจ้งผู้ดูแลระบบ",
	}
}
