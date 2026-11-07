-- A rollback must not silently destroy monthly progress or payment history.
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM monthly_payout_batches) OR EXISTS (SELECT 1 FROM document_progress WHERE fiscal_round > 2) OR EXISTS (SELECT 1 FROM signature_checklist WHERE fiscal_round > 2) THEN
  RAISE EXCEPTION 'Monthly payout records exist; archive them before rolling back';
 END IF;
END $$;
DROP TABLE monthly_payout_signers;
DROP TABLE monthly_payout_batches;
DROP TABLE payout_legacy_months;
ALTER TABLE document_progress DROP CONSTRAINT document_progress_fiscal_round_check;
ALTER TABLE document_progress ADD CONSTRAINT document_progress_fiscal_round_check CHECK(fiscal_round IN (1,2));
ALTER TABLE signature_checklist DROP CONSTRAINT signature_checklist_fiscal_round_check;
ALTER TABLE signature_checklist ADD CONSTRAINT signature_checklist_fiscal_round_check CHECK(fiscal_round IN (1,2));
