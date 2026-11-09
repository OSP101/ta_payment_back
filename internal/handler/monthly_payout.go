package handler

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"log"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/service"
)

// MonthlyPayoutZip issues one combined folder, only after the deadline and
// after ALL its courses are ready. No unfinished ZIP is released to the user.
func (h *ExportHandler) MonthlyPayoutZip(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(400, "invalid term")
	}
	months := monthsParam(c)
	if len(months) != 1 {
		return service.Invalid("กรุณาเลือกเดือนที่จะเบิกหนึ่งเดือน")
	}
	ym := months[0]
	all, err := h.Svc.Export.TermMonths(c.Context(), termID)
	if err != nil {
		return err
	}
	valid := false
	for _, m := range all {
		if m.YearMonth == ym {
			valid = true
		}
	}
	if !valid {
		return service.Invalid("เดือนไม่อยู่ในภาคเรียนนี้")
	}
	legacy, err := h.Svc.Export.LegacyPayoutMonths(c.Context(), termID)
	if err != nil {
		return err
	}
	if legacy[ym] {
		return service.Invalid("เดือนนี้มีเอกสารเดิมแล้ว กรุณาดำเนินการผ่านประวัติเอกสารเดิม")
	}
	// One issuer per term, across server instances: different months draw on
	// the same course budgets and must not build against stale frozen balances.
	// Do not hold a work_logs lock
	// during generation; each course checks its pre-build fingerprint on freeze.
	conn, err := h.Svc.Pool.Acquire(c.Context())
	if err != nil {
		return err
	}
	defer conn.Release()
	lockKey := "monthly-payout/" + termID.String()
	var locked bool
	if err = conn.QueryRow(c.Context(), `SELECT pg_try_advisory_lock(hashtextextended($1,0))`, lockKey).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return service.Conflict("กำลังออกชุดเบิกของภาคเรียนนี้อยู่ กรุณารอสักครู่")
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, e := conn.Exec(unlockCtx, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, lockKey); e != nil {
			_ = conn.Conn().Close(unlockCtx)
		}
	}()
	// An unchanged, locked month reprints the SAME combined archive.
	var oldPath, oldName string
	var oldVersion int
	var originalCourses []uuid.UUID
	var originalAt time.Time
	err = conn.QueryRow(c.Context(), `SELECT file_path,file_name,version,course_ids,generated_at FROM monthly_payout_batches WHERE term_id=$1 AND year_month=$2 ORDER BY version DESC LIMIT 1`, termID, ym).Scan(&oldPath, &oldName, &oldVersion, &originalCourses, &originalAt)
	if err != nil && err != pgx.ErrNoRows {
		return err
	}
	if oldPath != "" {
		same, e := h.Svc.Export.MonthlyArchiveCurrent(c.Context(), ym, originalAt, originalCourses)
		if e != nil {
			return e
		}
		if same {
			reader, e := h.Svc.Storage.Open(oldPath)
			if e != nil {
				return e
			}
			defer reader.Close()
			body, e := io.ReadAll(reader)
			if e != nil {
				return e
			}
			actor := UserID(c)
			if e = h.Svc.Auditor.Log(c.Context(), audit.Entry{ActorID: &actor, Action: "export.month_reprint", Entity: "academic_term", EntityID: termID.String(), After: map[string]any{"month": ym}}); e != nil {
				return e
			}
			c.Set("Content-Type", "application/zip")
			c.Set("Content-Disposition", contentDisposition("attachment", oldName))
			return c.Send(body)
		}
	}
	ids, err := h.Svc.Export.PayoutMonthCourses(c.Context(), termID, ym)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return service.Invalid("เดือนนี้ไม่มีรายการเบิกจ่าย")
	}
	closed, err := h.Svc.Export.MonthlyPayoutClosed(c.Context(), termID, ym)
	if err != nil {
		return err
	}
	if !closed {
		return service.Invalid("ยังไม่สิ้นสุดกรอบส่งเวลา จึงยังออกชุดเบิกประจำเดือนไม่ได้")
	}
	type coursePack struct {
		id    uuid.UUID
		pack  *service.CourseZip
		fp    string
		body  []byte
		name  string
		path  string
		total float64
	}
	packs := []coursePack{}
	var createdKeys []string
	commitAttempted := false
	defer func() {
		// A failed COMMIT can be ambiguous (e.g. lost network acknowledgement).
		// Keep those artifacts for reconciliation; never delete a potentially
		// committed financial archive on a client/connection error.
		if commitAttempted { return }
		for _, key := range createdKeys {
			if err := h.Svc.Storage.Delete(key); err != nil { log.Printf("uncommitted payout artifact cleanup failed") }
		}
	}()
	// Preflight every course before the first lock. A pending lecturer, profile,
	// appointment, or staff review on any course blocks the combined month.
	blockers := []service.ExportBlocker{}
	for _, id := range ids {
		bs, e := h.Svc.Export.CourseExportBlockers(c.Context(), id, months)
		if e != nil {
			return e
		}
		blockers = append(blockers, bs...)
	}
	for _, level := range []string{"undergrad", "graduate"} {
		bs, e := h.Svc.Export.TermExportBlockers(c.Context(), termID, months, level)
		if e != nil {
			return e
		}
		for _, b := range bs {
			if b.Kind != "not_exported" {
				blockers = append(blockers, b)
			}
		}
	}
	if len(blockers) > 0 {
		return service.Invalid("ยังตรวจสอบรายการของเดือนนี้ไม่ครบ กรุณาตรวจรายวิชาที่ค้างก่อนออกชุดเบิกรวม")
	}

	scope := 0
	fmt.Sscanf(strings.ReplaceAll(ym, "-", ""), "%d", &scope)
	signers, err := h.Svc.DocProgress.MonthlySignerCandidates(c.Context(), termID, scope)
	if err != nil {
		return err
	}
	var buf payoutArchiveBuffer
	zw := zip.NewWriter(&buf)
	var total float64
	for _, id := range ids {
		cp := coursePack{id: id}
		archive, e := h.Svc.Export.ArchivedReissue(c.Context(), id, months)
		if e != nil {
			return e
		}
		if archive != nil {
			cp.body = archive.Body
			cp.name = archive.Batch.FileName
			cp.total = archive.Batch.TotalBaht
		} else {
			cp.fp, e = h.Svc.SubmissionPeriods.CourseWorklogFingerprint(c.Context(), id, months)
			if e != nil {
				return e
			}
			cp.pack, e = h.Svc.Export.BuildCourseZipPack(c.Context(), id, months)
			if e != nil {
				return e
			}
			cp.body = cp.pack.Body
			cp.name = cp.pack.Name
			preview, e := h.Svc.Export.CoursePreview(service.WithGradLumpSnapshot(c.Context(), cp.pack.GradLumps), id, months)
			if e != nil {
				return e
			}
			cp.total = preview.TotalActual
			cp.path, _, e = h.Svc.Storage.Save("export_batches", cp.name, bytes.NewReader(cp.body))
			if e != nil {
				return e
			}
			createdKeys = append(createdKeys,cp.path)
		}
		w, e := zw.Create(id.String() + "/" + cp.name)
		if e != nil {
			return e
		}
		if _, e = w.Write(cp.body); e != nil {
			return e
		}
		cp.body = nil
		if cp.pack != nil { cp.pack.Body = nil }
		total += cp.total
		packs = append(packs, cp)
	}
	total = math.Round(total*100) / 100
	manifest, _ := json.MarshalIndent(map[string]any{"year_month": ym, "term_id": termID, "course_ids": ids, "total_baht": total, "version": oldVersion + 1}, "", "  ")
	w, err := zw.Create("manifest.json")
	if err != nil {
		return err
	}
	if _, err = w.Write(manifest); err != nil {
		return err
	}
	if err = zw.Close(); err != nil {
		return err
	}
	name := fmt.Sprintf("TA-payout-%s-v%d.zip", ym, oldVersion+1)
	path, _, err := h.Svc.Storage.Save("monthly_payout_batches", name, bytes.NewReader(buf.Bytes()))
	if err != nil {
		return err
	}
	createdKeys = append(createdKeys,path)
	tx, err := h.Svc.Pool.Begin(c.Context())
	if err != nil {
		return err
	}
	defer tx.Rollback(c.Context())

	actor := UserID(c)
	var actorName string
	if err = tx.QueryRow(c.Context(), `SELECT first_name||' '||last_name FROM users WHERE id=$1`, actor).Scan(&actorName); err != nil {
		return err
	}
	// All figures, locks, course archive rows, and the month marker commit
	// together. Failure on the LAST course rolls back the FIRST course too.
	for _, cp := range packs {
		if cp.pack == nil {
			continue
		}
		if _, err = h.Svc.SubmissionPeriods.MarkCourseExportedInTx(c.Context(), tx, actor, cp.id, months, cp.fp, cp.pack.Figures, actorName); err != nil {
			return err
		}
		if err = h.Svc.Export.FreezeGradLumpSnapshotTx(c.Context(), tx, actor, cp.pack.GradLumps, months); err != nil {
			return err
		}
		if _, err = h.Svc.ExportBatches.RecordTx(c.Context(), tx, actor, service.ExportBatch{TeachingCourseID: cp.id, FilePath: cp.path, FileName: cp.name, TACount: cp.pack.TACount, Months: months, TotalBaht: cp.total}); err != nil {
			return err
		}
		if _, err = tx.Exec(c.Context(), `UPDATE teaching_courses SET exported_at=COALESCE(exported_at,now()) WHERE id=$1`, cp.id); err != nil {
			return err
		}
	}
	var version int
	if err = tx.QueryRow(c.Context(), `SELECT COALESCE(max(version),0)+1 FROM monthly_payout_batches WHERE term_id=$1 AND year_month=$2`, termID, ym).Scan(&version); err != nil {
		return err
	}
	batchID := uuid.New()
	if _, err = tx.Exec(c.Context(), `INSERT INTO monthly_payout_batches(id,term_id,year_month,version,file_path,file_name,course_ids,total_baht,generated_by) VALUES($9,$1,$2,$3,$4,$5,$6,$7,$8)`, termID, ym, version, path, name, ids, total, actor, batchID); err != nil {
		return err
	}
	for _, it := range signers {
		if _, err = tx.Exec(c.Context(), `INSERT INTO monthly_payout_signers(batch_id,teaching_course_id,code,name_th,role,signer_id,responsible) VALUES($1,$2,$3,$4,$5,$6,$7)`, batchID, it.TeachingCourseID, it.Code, it.NameTH, it.Role, it.SignerID, it.Responsible); err != nil {
			return err
		}
	}

	// Corrected documents require new signatures. Keep the previous checklist
	// and stage as an audit before-image; archived files/versions are retained.
	if version > 1 {
		var previous []byte
		scope := strings.ReplaceAll(ym, "-", "")
		if err = tx.QueryRow(c.Context(), `SELECT json_build_object('progress',(SELECT row_to_json(p) FROM document_progress p WHERE term_id=$1 AND fiscal_round=$2::int),'signatures',(SELECT json_agg(s) FROM signature_checklist s WHERE term_id=$1 AND fiscal_round=$2::int))`, termID, scope).Scan(&previous); err != nil {
			return err
		}
		if err = h.Svc.Auditor.LogTx(c.Context(), tx, audit.Entry{ActorID: &actor, Action: "export.month_revision", Entity: "academic_term", EntityID: termID.String(), Before: json.RawMessage(previous), After: map[string]any{"month": ym, "version": version}}); err != nil {
			return err
		}

		if _, err = tx.Exec(c.Context(), `DELETE FROM signature_checklist WHERE term_id=$1 AND fiscal_round=$2::int`, termID, scope); err != nil {
			return err
		}
		if _, err = tx.Exec(c.Context(), `DELETE FROM document_progress WHERE term_id=$1 AND fiscal_round=$2::int`, termID, scope); err != nil {
			return err
		}
	}
	if err = h.Svc.Auditor.LogTx(c.Context(), tx, audit.Entry{ActorID: &actor, Action: "export.month", Entity: "academic_term", EntityID: termID.String(), After: map[string]any{"month": ym, "version": version, "total_baht": total, "course_ids": ids}}); err != nil {
		return err
	}
	commitAttempted = true
	if err = tx.Commit(c.Context()); err != nil {
		return err
	}
	notified := map[uuid.UUID]bool{}
	for _, it := range signers {
		if it.Role == "ta" && it.SignerID != nil && !notified[*it.SignerID] {
			notified[*it.SignerID] = true
			h.Svc.Notify.SendAction(c.Context(), *it.SignerID, "จัดทำชุดเบิกค่าตอบแทนประจำเดือน "+ym, fmt.Sprintf("เจ้าหน้าที่ออกชุดเบิกค่าตอบแทนประจำเดือน %s ฉบับที่ %d แล้ว สามารถติดตามความคืบหน้าเอกสารได้", ym, version), "/document-progress")
		}
	}
	c.Set("Content-Type", "application/zip")
	c.Set("Content-Disposition", contentDisposition("attachment", name))
	return c.Send(buf.Bytes())
}

// Combined archives are buffered for encryption/storage. Bound that buffer;
// never release a partial archive or financial transaction on overflow.
type payoutArchiveBuffer struct { bytes.Buffer }
func (b *payoutArchiveBuffer) Write(p []byte) (int,error) {
	if b.Len()+len(p)>64*1024*1024 { return 0,fiber.NewError(413,"ชุดเอกสารมีขนาดใหญ่เกินไป กรุณาติดต่อผู้ดูแลระบบ") }
	return b.Buffer.Write(p)
}
