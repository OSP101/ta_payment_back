package service

// A/D rows may include both an original-time reference and the actual moved
// sitting in the same document. The user confirmed on 2026-11-27 that the moved
// sitting wins. A zero-duration obsolete row is also reference-only when the
// same original/class/document has a positive-duration moved replacement.
// Never guess between two genuinely different positive moved replacements.
func tdbmNormalizeReferences(in []tdbmExtraTeachingRow) []tdbmExtraTeachingRow {
	out := append([]tdbmExtraTeachingRow(nil), in...)
	type key struct{ teaching, class, document int }
	moved := map[key]bool{}
	positive := map[key]bool{}
	rowKey := func(r tdbmExtraTeachingRow) (key, bool) {
		if r.TeachingID == nil || r.ClassID == nil || r.DocumentID == nil || r.OriginalDate == nil || r.OriginalStart == nil || r.OriginalEnd == nil {
			return key{}, false
		}
		return key{*r.TeachingID, *r.ClassID, *r.DocumentID}, true
	}
	unchanged := func(r tdbmExtraTeachingRow) bool {
		return r.ClassDate == *r.OriginalDate && r.StartTime == *r.OriginalStart && r.EndTime == *r.OriginalEnd
	}
	for _, r := range out {
		if k, ok := rowKey(r); ok && tdbmMakeupEligible(r.OptStatus) && r.SourceDuration != nil && *r.SourceDuration > 0 {
			positive[k] = true
			if !unchanged(r) {
				moved[k] = true
			}
		}
	}
	for i := range out {
		r := &out[i]
		r.Reference = false
		if k, ok := rowKey(*r); ok && tdbmMakeupEligible(r.OptStatus) {
			// A source edit may restore the original sitting and zero the old
			// moved row. Use its positive current replacement in either case.
			r.Reference = (positive[k] && r.SourceDuration != nil && *r.SourceDuration == 0) || (moved[k] && unchanged(*r))
		}
	}
	return out
}
