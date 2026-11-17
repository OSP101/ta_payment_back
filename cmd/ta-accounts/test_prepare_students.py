import unittest
from prepare_students import prepare, legacy_identities


def student(sid="693380001-1", email="synthetic@example.invalid", **extra):
    return {"รหัสนักศึกษา": sid, "ชื่อ-นามสกุล": "นางสาวทดสอบ นำเข้า",
            "อีเมล": email, "หลักสูตร": "Computer Science (SC-CS)",
            "สถานะ": "เปิดใช้งาน", **extra}


class PrepareStudentsTest(unittest.TestCase):
    def test_history_is_not_inferred_from_being_a_student(self):
        first = student()
        second = student("693380002-1", "legacy@example.invalid")
        history = legacy_identities([{"student_id": second["รหัสนักศึกษา"], "email": second["อีเมล"]}])
        ready, review = prepare([first, second], history)
        self.assertEqual(review, [])
        self.assertNotIn("ta_seniority", ready[0])
        self.assertEqual(ready[1]["ta_seniority"], "returning")

    def test_shared_and_missing_emails_preserve_all_source_rows_for_review(self):
        ready, review = prepare([student(), student("693380002-1"), student("693380003-1", "")], {})
        self.assertEqual(ready, [])
        self.assertEqual(len(review), 3)
        self.assertIn("email_shared_by_multiple_students", review[0]["reasons"])
        self.assertIn("missing_or_invalid_email", review[2]["reasons"])

    def test_inactive_and_program_data_are_preserved(self):
        ready, review = prepare([student(**{"สถานะ": "ปิดใช้งาน"})], {})
        self.assertEqual(review, [])
        self.assertFalse(ready[0]["is_active"])
        self.assertEqual(ready[0]["department"], "Computer Science (SC-CS)")

    def test_legacy_identity_requires_both_id_and_email(self):
        ready, _ = prepare([student()], {(student()["รหัสนักศึกษา"], "another@example.invalid"): {"levels": ["phd"]}})
        self.assertNotIn("ta_seniority", ready[0])
        self.assertEqual(ready[0]["study_level"], "undergrad")

    def test_unknown_degree_is_reviewed_and_generic_cs_phd_is_recognized(self):
        ready, review = prepare([student(**{"หลักสูตร": "unknown"}), student("697380001-1", "phd@example.invalid")], {})
        self.assertEqual(len(review), 1)
        self.assertEqual(ready[0]["study_level"], "phd")


if __name__ == "__main__":
    unittest.main()
