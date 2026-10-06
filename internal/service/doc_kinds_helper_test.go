package service

// requiredDocKinds is what a Thai TA owes (ta_required_doc_kinds, 0151) — the
// set most fixtures build. foreignRequiredDocKinds is the foreign TA's.
var (
	requiredDocKinds        = []string{"national_id", "bank_book", "creditor_form"}
	foreignRequiredDocKinds = []string{"passport", "bank_book", "creditor_form"}
)
