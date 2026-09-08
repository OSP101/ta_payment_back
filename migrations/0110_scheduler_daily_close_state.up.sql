-- ติดตามว่า dailyClose รันครั้งล่าสุดวันไหน — OPS-01
--
-- เดิม dailyClose ถูกยิงด้วย time.NewTicker(24 * time.Hour) ที่รีเซ็ตทุกครั้ง
-- ที่โปรเซสรีสตาร์ท ⇒ deployment ที่ deploy ถี่กว่าวันละครั้งจะไม่เคยรัน
-- dailyClose เลยแม้แต่ครั้งเดียว ตารางนี้แทนที่ ticker ด้วยการเช็คทุกชั่วโมง
-- ว่า "วันนี้ (ตามเขต Asia/Bangkok) รันไปหรือยัง" ซึ่งทนต่อการรีสตาร์ทได้จริง
--
-- แถวเดียวเสมอ (id คงที่) — ไม่ต้องมีคีย์อื่นเพราะ dailyClose เป็นงานระดับ
-- โปรเซสเดียว ไม่ผูกกับ term/course ใด ๆ
CREATE TABLE scheduler_daily_close_state (
    id           BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    last_run_at  TIMESTAMPTZ NOT NULL
);
