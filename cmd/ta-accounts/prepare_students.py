"""Prepare all student accounts; retain ambiguous identities in a review file."""
import argparse
from collections import Counter
import json
from pathlib import Path
import re

UG_PROGRAMS = {
    "Computer Science (SC-CS)",
    "Information Technology and Innovation Intelligence (SC-IT)",
    "Artificial Intelligence (CP-AI)", "Cybersecurity (CP-Cy)",
    "Geo-Informatics (SC-GIS)", "CP-GIS",
}
MASTER_PROGRAMS = {
    "Computer Science and Information Technology แผน ข (Com&It 2)",
    "Computer Science and Information Technology แผน ก (Com&It A2)",
}
PHD_PROGRAM = "Computer Science and Information Technology (แผน 1.1) (IT 1.1)"


def legacy_identities(data):
    people = data.get("people", []) if isinstance(data, dict) else data
    result = {}
    for p in people:
        if isinstance(data, dict) and not p.get("matched"):
            continue
        sid, email = p.get("student_id", ""), p.get("email", "").strip().lower()
        if sid and email:
            result[(sid, email)] = p
    return result


def study_level(sid, program, legacy):
    levels = legacy.get("levels", [])
    if not levels and legacy.get("study_level"):
        levels = [legacy["study_level"]]
    if len(levels) == 1 and levels[0] in {"undergrad", "master", "phd"}:
        return levels[0]
    if program in MASTER_PROGRAMS and sid[2:3] == "5":
        return "master"
    if (program == PHD_PROGRAM or program == "Computer Science (SC-CS)") and sid[2:3] == "7":
        return "phd"
    if program in UG_PROGRAMS and sid[2:3] in {"3", "0"}:
        return "undergrad"
    return ""


def prepare(rows, legacy):
    emails = Counter(str(r.get("อีเมล") or "").strip().lower() for r in rows)
    ids = Counter(str(r.get("รหัสนักศึกษา") or "").strip() for r in rows)
    ready, review = [], []
    for row_number, r in enumerate(rows, 2):
        sid = str(r.get("รหัสนักศึกษา") or "").strip()
        email = str(r.get("อีเมล") or "").strip().lower()
        name = str(r.get("ชื่อ-นามสกุล") or "").strip()
        program = str(r.get("หลักสูตร") or "").strip()
        status = r.get("สถานะ")
        history = legacy.get((sid, email), {})
        level = study_level(sid, program, history)
        reasons = []
        if not re.fullmatch(r"\d{9}-\d", sid) or ids[sid] != 1:
            reasons.append("invalid_or_duplicate_student_id")
        if not re.fullmatch(r"[^\s@]+@[^\s@]+\.[^\s@]+", email):
            reasons.append("missing_or_invalid_email")
        elif emails[email] != 1:
            reasons.append("email_shared_by_multiple_students")
        if status not in {"เปิดใช้งาน", "ปิดใช้งาน"}:
            reasons.append("unknown_active_status")
        if not level:
            reasons.append("study_level_requires_review")
        title = ""
        for prefix in ("นางสาว", "นาย", "นาง", "น.ส."):
            if name.startswith(prefix):
                title, name = prefix, name[len(prefix):].strip()
                break
        parts = name.split(maxsplit=1)
        if len(parts) != 2 or not all(parts):
            reasons.append("incomplete_name")
        if reasons:
            review.append({"source_row": row_number, "student_id": sid, "email": email,
                           "name": r.get("ชื่อ-นามสกุล"), "program": program,
                           "status": status, "reasons": reasons})
            continue
        account = {"student_id": sid, "email": email, "title": title,
                   "first_name": parts[0], "last_name": parts[1], "study_level": level,
                   "department": program, "is_active": status == "เปิดใช้งาน"}
        if history:
            account.update(ta_seniority="returning", history_source="TA ระบบเดิม ภาคต้น ปีการศึกษา 2569")
        ready.append(account)
    return ready, review


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("source", type=Path)
    parser.add_argument("output", type=Path)
    parser.add_argument("--legacy", type=Path, required=True)
    args = parser.parse_args()
    from openpyxl import load_workbook  # read-only extraction, no XLSX authoring
    workbook = load_workbook(args.source, read_only=True, data_only=True)
    values = iter(workbook.active.values)
    headers = next(values)
    rows = [dict(zip(headers, r)) for r in values if any(v is not None for v in r)]
    legacy = legacy_identities(json.loads(args.legacy.read_text()))
    ready, review = prepare(rows, legacy)
    assert len(ready) + len(review) == len(rows)
    summary = {"source_file": args.source.name, "source_rows": len(rows),
               "ready_rows": len(ready), "review_rows": len(review),
               "ready_active": sum(r["is_active"] for r in ready),
               "ready_inactive": sum(not r["is_active"] for r in ready),
               "verified_legacy_tas": sum(r.get("ta_seniority") == "returning" for r in ready),
               "levels": dict(Counter(r["study_level"] for r in ready)),
               "review_reasons": dict(Counter(k for r in review for k in r["reasons"]))}
    args.output.parent.mkdir(parents=True, exist_ok=True)
    for path, data in [(args.output, ready), (args.output.with_suffix(".review.json"), review),
                       (args.output.with_suffix(".summary.json"), summary)]:
        path.write_text(json.dumps(data, ensure_ascii=False, indent=2) + "\n")
    print(json.dumps(summary, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
