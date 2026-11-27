package service

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"strconv"
	"time"

	"github.com/go-sql-driver/mysql"
	"ta-payment-back/internal/config"
)

type tdbmSnapshot struct {
	StartDate string
	EndDate   string
	Holidays  []tdbmHolidayRow
	Extra     []tdbmExtraTeachingRow
}

type tdbmSource interface {
	Snapshot(context.Context, int, int) (tdbmSnapshot, error)
}

type tdbmMariaDBSource struct{ cfg config.Config }

// Each pull owns one short-lived connection and one read-only, repeatable-read
// transaction. No upstream data or schema writes are issued by this connector.
func (s *tdbmMariaDBSource) Snapshot(ctx context.Context, year, semester int) (out tdbmSnapshot, err error) {
	if s.cfg.TDBMDBHost == "" || s.cfg.TDBMDBUser == "" || s.cfg.TDBMDBPassword == "" {
		return out, errors.New("TDBM MariaDB connection is not configured")
	}
	cfg := mysql.NewConfig()
	cfg.User, cfg.Passwd, cfg.DBName = s.cfg.TDBMDBUser, s.cfg.TDBMDBPassword, s.cfg.TDBMDBName
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(s.cfg.TDBMDBHost, strconv.Itoa(s.cfg.TDBMDBPort))
	cfg.Timeout, cfg.ReadTimeout, cfg.WriteTimeout = 10*time.Second, 30*time.Second, 10*time.Second
	cfg.TLSConfig = s.cfg.TDBMDBTLS
	// DATE/DATETIME are formatted by SQL; timestamp audit values use UTC,
	// matching the upstream server. Teaching wall-clock values stay unchanged.
	cfg.Params = map[string]string{"time_zone": "'+00:00'"}
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return out, errors.New("invalid TDBM MariaDB configuration")
	}
	db := sql.OpenDB(connector)
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return out, errors.New("cannot read TDBM MariaDB")
	}
	defer tx.Rollback()
	// A missing term is an error, never an empty authoritative snapshot.
	err = tx.QueryRowContext(ctx, `SELECT DATE_FORMAT(start_date,'%Y-%m-%d'), DATE_FORMAT(end_date,'%Y-%m-%d')
	 FROM semesters WHERE year=? AND semester=?`, year, semester).Scan(&out.StartDate, &out.EndDate)
	if err != nil || out.StartDate == "" || out.EndDate == "" || out.StartDate > out.EndDate {
		return out, errors.New("TDBM academic term is unavailable or invalid")
	}
	rows, err := tx.QueryContext(ctx, `SELECT holiday_id, DATE_FORMAT(h_date,'%Y-%m-%d'),
	 COALESCE(title,''), h_type, status FROM holidays WHERE h_date BETWEEN ? AND ? ORDER BY holiday_id`, out.StartDate, out.EndDate)
	if err != nil {
		return out, errors.New("cannot read TDBM holidays")
	}
	for rows.Next() {
		var r tdbmHolidayRow
		if err = rows.Scan(&r.HolidayID, &r.HDate, &r.Title, &r.HType, &r.Status); err != nil {
			rows.Close()
			return out, errors.New("invalid TDBM holiday row")
		}
		out.Holidays = append(out.Holidays, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, errors.New("incomplete TDBM holiday snapshot")
	}
	rows, err = tx.QueryContext(ctx, `SELECT e.extra_class_id, e.title,
	 COALESCE(DATE_FORMAT(e.class_date,'%Y-%m-%d'),''),
	 COALESCE(TIME_FORMAT(e.start_time,'%H:%i:%s'),''), COALESCE(TIME_FORMAT(e.end_time,'%H:%i:%s'),''),
	 COALESCE(TIMESTAMPDIFF(MINUTE,CONCAT(e.class_date,' ',e.start_time),CONCAT(e.class_date,' ',e.end_time)),0),
	 co.subject_id, CAST(cl.section_num AS CHAR),
	 CASE ma.major_type WHEN 'N' THEN 'ภาคปกติ' WHEN 'S' THEN 'ภาคพิเศษ' ELSE NULL END,
	 teacher.name, e.opt_status, e.teaching_id, e.class_id, e.holiday_id,
	 DATE_FORMAT(t.start_time,'%Y-%m-%d'), TIME_FORMAT(t.start_time,'%H:%i:%s'), TIME_FORMAT(t.end_time,'%H:%i:%s'),
	 CASE t.class_type WHEN 'C' THEN 'lecture' WHEN 'L' THEN 'lab' ELSE NULL END,
	 (SELECT h.et_status_id FROM status_histories h WHERE h.extra_class_id=e.extra_class_id ORDER BY h.created_at DESC,h.id DESC LIMIT 1),
	 DATE_FORMAT(e.updated_at,'%Y-%m-%dT%H:%i:%sZ'),
	 (SELECT DATE_FORMAT(MAX(h.created_at),'%Y-%m-%dT%H:%i:%sZ') FROM status_histories h WHERE h.extra_class_id=e.extra_class_id),e.etdoc_id,e.duration
	 FROM extra_teaching e JOIN classes cl ON cl.class_id=e.class_id
	 JOIN semesters sem ON sem.semester_id=cl.semester_id
	 LEFT JOIN courses co ON co.course_id=cl.course_id
	 LEFT JOIN majors ma ON ma.major_id=cl.major_id
	 LEFT JOIN teachers teacher ON teacher.teacher_id=co.owner_teacher_id
	 LEFT JOIN teaching t ON t.teaching_id=e.teaching_id AND t.class_id=e.class_id
	 WHERE sem.year=? AND sem.semester=? ORDER BY e.extra_class_id`, year, semester)
	if err != nil {
		return out, errors.New("cannot read TDBM makeup teachings")
	}
	for rows.Next() {
		var r tdbmExtraTeachingRow
		if err = rows.Scan(&r.ExtraClassID, &r.Title, &r.ClassDate, &r.StartTime, &r.EndTime,
			&r.Duration, &r.CourseCode, &r.Section, &r.SemesterType, &r.OwnerTeacherName, &r.OptStatus,
			&r.TeachingID, &r.ClassID, &r.HolidayID, &r.OriginalDate, &r.OriginalStart, &r.OriginalEnd,
			&r.OriginalKind, &r.LatestStatusID, &r.SourceUpdatedAt, &r.StatusUpdatedAt, &r.DocumentID, &r.SourceDuration); err != nil {
			rows.Close()
			return out, errors.New("invalid TDBM makeup row")
		}
		out.Extra = append(out.Extra, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, errors.New("incomplete TDBM makeup snapshot")
	}
	// This legacy table currently has no data. Do not silently apply only
	// part of a multi-part replacement if the upstream begins using it.
	var parts int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM extra_teaching_times x
	 JOIN extra_teaching e ON e.extra_class_id=x.extra_teaching_id
	 JOIN classes cl ON cl.class_id=e.class_id JOIN semesters sem ON sem.semester_id=cl.semester_id
	 WHERE sem.year=? AND sem.semester=? AND e.opt_status IN ('A','D')`, year, semester).Scan(&parts)
	if err != nil || parts > 0 {
		return out, errors.New("TDBM multi-part replacement snapshot is unsupported or unavailable")
	}
	if err = tx.Commit(); err != nil {
		return out, errors.New("cannot complete TDBM read snapshot")
	}
	return out, nil
}

func (s *TDBMService) snapshot(ctx context.Context, year, semester int) (tdbmSnapshot, error) {
	if s.source == nil {
		return tdbmSnapshot{}, errors.New("TDBM MariaDB source is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	return s.source.Snapshot(ctx, year, semester)
}
