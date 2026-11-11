"""Prepare account-only input from the extracted legacy staging JSON."""
import argparse
import json
import re
from collections import Counter
from pathlib import Path

parser = argparse.ArgumentParser()
parser.add_argument("source", type=Path)
parser.add_argument("output", type=Path)
args = parser.parse_args()
data = json.loads(args.source.read_text())
ready, excluded = [], []
for person in data["people"]:
    reasons = []
    name = person["student_name"].strip()
    email = person["email"].strip().lower()
    if not person["matched"]:
        reasons.append("student_id_not_matched")
    if not re.fullmatch(r"[^\s@]+@[^\s@]+\.[^\s@]+", email):
        reasons.append("missing_or_invalid_verified_email")
    levels = person["levels"]
    if len(levels) != 1 or levels[0] not in ("undergrad", "master", "phd"):
        reasons.append("study_level_missing_or_conflicting")
    title = ""
    for prefix in ("นางสาว", "นาย", "นาง", "น.ส."):
        if name.startswith(prefix):
            title, name = prefix, name[len(prefix):].strip()
            break
    parts = name.split(maxsplit=1)
    if len(parts) != 2 or not all(parts):
        reasons.append("verified_name_incomplete")
    records = [r for r in data["assignments"] if r["student_id"] == person["student_id"] and r["email"]]
    if records and any(r["student_status"] != "เปิดใช้งาน" for r in records):
        reasons.append("student_export_not_active")
    if reasons:
        excluded.append({"student_id": person["student_id"], "reasons": reasons})
    else:
        ready.append({"student_id": person["student_id"], "email": email, "title": title,
                      "first_name": parts[0], "last_name": parts[1], "study_level": levels[0]})
emails = Counter(r["email"] for r in ready)
if any(n > 1 for n in emails.values()):
    raise ValueError("Duplicate ready email; resolve before import")
args.output.parent.mkdir(parents=True, exist_ok=True)
args.output.write_text(json.dumps(ready, ensure_ascii=False, indent=2))
args.output.with_suffix(".excluded.json").write_text(json.dumps(excluded, ensure_ascii=False, indent=2))
print(f"Ready: {len(ready)}; excluded: {len(excluded)}")
