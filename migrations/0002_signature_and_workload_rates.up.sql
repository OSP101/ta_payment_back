-- Store signature as PNG base64 for embedding into DOCX exports.
ALTER TABLE ta_profiles ADD COLUMN IF NOT EXISTS signature_png_b64 TEXT;

-- Add per-workload monthly rates + hours-per-credit constants matching the
-- undergrad calculation formula in the historical Excel workbook (แท็บ "2_59 ป.ตรี").
ALTER TABLE pay_rates ADD COLUMN IF NOT EXISTS ug_lecture_hours_per_credit NUMERIC(4,2) NOT NULL DEFAULT 3;
ALTER TABLE pay_rates ADD COLUMN IF NOT EXISTS ug_lab_hours_per_credit     NUMERIC(4,2) NOT NULL DEFAULT 4.5;
ALTER TABLE pay_rates ADD COLUMN IF NOT EXISTS baseline_students_lecture   INT NOT NULL DEFAULT 60;
ALTER TABLE pay_rates ADD COLUMN IF NOT EXISTS baseline_students_lab       INT NOT NULL DEFAULT 30;
-- monthly rate per weekly-workload-hour (undergrad); grad uses lump-sum only.
ALTER TABLE pay_rates ADD COLUMN IF NOT EXISTS ug_workload_rate_regular    NUMERIC(10,2) NOT NULL DEFAULT 200;
ALTER TABLE pay_rates ADD COLUMN IF NOT EXISTS ug_workload_rate_special    NUMERIC(10,2) NOT NULL DEFAULT 250;
-- term length in months for total-budget calc (typically 4)
ALTER TABLE pay_rates ADD COLUMN IF NOT EXISTS term_months                 INT NOT NULL DEFAULT 4;
