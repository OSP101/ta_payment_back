-- แยก "อนุมัติแล้ว" ออกจาก "ลบไฟล์เสร็จจริงแล้ว"
-- เดิมมีแค่ executed_at ซึ่งถูกตั้งใน transaction เดียวกับที่อนุมัติ ก่อนที่
-- ScrubUserDocuments จะได้รันด้วยซ้ำ ⇒ คำขอที่ลบไฟล์ไม่สำเร็จก็ยังอ่านว่าเสร็จ
ALTER TABLE data_deletion_requests
  ADD COLUMN scrub_completed_at TIMESTAMPTZ,
  ADD COLUMN scrub_error        TEXT,
  ADD COLUMN scrub_attempts     INT NOT NULL DEFAULT 0,
  -- users.avatar_key is nulled out in the SAME transaction that approves the
  -- request (the row must stop pointing at a live avatar immediately,
  -- independent of whether the blob delete succeeds) — copied here first so
  -- a later sweeper retry still knows which blob it owes a delete.
  ADD COLUMN avatar_key_to_scrub TEXT;

COMMENT ON COLUMN data_deletion_requests.scrub_completed_at IS
  'เวลาที่ blob เอกสารและ avatar ถูกลบออกจาก storage สำเร็จจริง — NULL ทั้งที่ '
  'status=approved แปลว่ายังลบไม่สำเร็จ และ sweeper จะพยายามซ้ำ';
