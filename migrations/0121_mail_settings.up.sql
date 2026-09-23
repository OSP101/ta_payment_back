-- Editable contact block at the foot of every notification e-mail.
--
-- One row (id is always TRUE). Staff edit it under ตั้งค่า > อีเมลแจ้งเตือน so
-- the unit name, room, phone and e-mail can change without a deploy.
CREATE TABLE IF NOT EXISTS mail_settings (
    id              BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    contact_heading TEXT NOT NULL,
    contact_unit    TEXT NOT NULL,
    -- Free lines under the unit: building/room, phone, e-mail. May be empty.
    contact_detail  TEXT NOT NULL DEFAULT '',
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by      UUID REFERENCES users(id)
);

INSERT INTO mail_settings (id, contact_heading, contact_unit, contact_detail)
VALUES (TRUE,
        'หากมีข้อสงสัยเพิ่มเติม สามารถติดต่อได้ที่',
        'เจ้าหน้าที่ระบบเบิกจ่ายค่าตอบแทนผู้ช่วยสอน (COCO TAS) วิทยาลัยการคอมพิวเตอร์ มหาวิทยาลัยขอนแก่น',
        '')
ON CONFLICT (id) DO NOTHING;
