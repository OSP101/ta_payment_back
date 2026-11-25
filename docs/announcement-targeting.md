# Announcement recipients and channels

The `ta` account role is available to students faculty-wide and does not establish TA membership. Announcement role targets now resolve TAs through assignments on lecturer requests with status `submitted` or `approved`, in the selected teaching course's academic term. Dropped assignments and draft, rejected, or cancelled requests do not qualify. Both trimmed and active assignments qualify. The same membership boundary applies to TA-specific condition filters, including when no role is selected.

Role, course and condition selections intersect. Named account recipients are an explicit addition and can include someone outside the selected term's cohort. Selecting every role includes the term's actual TAs and the other selected roles; it does not claim to select every student account.

`term_id` on audience previews and `target_term_id` on announcement upserts must describe the same cohort. The composer defaults to the active term, permits another term, and clears selected courses when that term changes. Changing a published announcement's term refreshes its recipient ledger. Recipient membership is otherwise frozen at publication, as before.

`send_email: false` publishes the announcement and delivers in-app notifications only. The setting also applies to retries and unread reminders. `true` delivers through the existing in-app and email path. Changing the setting does not automatically redeliver to recipients whose ledger already says sent. Pin payloads omit the setting and preserve it.

Apply migration `0163_announcement_delivery_channel` before starting the updated backend. Existing announcements and older clients that omit the setting retain email delivery; the new composer starts with in-app-only delivery selected.

Validation:

- Backend announcement regression tests use `TEST_DATABASE_URL` pointed at a disposable PostgreSQL server.
- Frontend: `npx tsc --noEmit`, `node scripts/i18n-check.mjs app/staff/announce`, and `node scripts/announcement-targeting.browser.test.mjs`.
