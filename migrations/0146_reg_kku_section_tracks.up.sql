-- Regular or special, per registrar class group, as the registrar's detail
-- page states it (reg.kku.ac.th). The search page — one request per code —
-- gives every group's enrolment but not its track. For a course's own code
-- the track comes from our sections; for a merged code (whose sections were
-- folded away, 0145) or a group we do not have, the detail page is read once
-- and remembered here, so later fetches stay at one request per code
-- (internal/regkku, RegEnrolmentService).
CREATE TABLE reg_kku_section_tracks (
    academic_year INT     NOT NULL,
    semester      INT     NOT NULL,
    code          TEXT    NOT NULL,
    sec_no        TEXT    NOT NULL, -- as the search page prints it: "1", no leading zero
    special       BOOLEAN NOT NULL,
    fetched_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (academic_year, semester, code, sec_no)
);
