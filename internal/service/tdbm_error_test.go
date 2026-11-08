package service

import "testing"

func TestPublicTDBMSyncError(t *testing.T) {
	warning := "ข้ามวันหยุดจาก TDBM 2 วัน (2026-11-09, 2026-11-10) เพราะมีบันทึกเวลาคาบบรรยาย/ปฏิบัติการที่ส่งหรืออนุมัติแล้วในวันนั้น ให้อาจารย์ตีกลับรายการเหล่านั้นก่อน แล้วซิงก์อีกครั้ง"
	for _, msg := range []string{
		"pq: password authentication failed for user payroll",
		"Get https://secret@example.invalid/private: connection refused",
		"อ่านข้อมูลไม่สำเร็จ: cannot scan NULL into private_column",
		"open /private/เอกสาร.pdf: permission denied",
		tdbmSyncFailureMessage,
		warning + " private driver details",
		"private details " + warning,
		warning + "\nprivate details",
	} {
		if got := publicTDBMSyncError(msg); got != tdbmSyncFailureMessage {
			t.Errorf("internal detail leaked: %q", got)
		}
	}
	for _, msg := range []string{"", warning} {
		if got := publicTDBMSyncError(msg); got != msg {
			t.Errorf("safe message changed: %q", got)
		}
	}
}
