package database

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/BushSchoolIT/extractor/blackbaud"
	"github.com/jackc/pgx/v5"
)

type State struct {
	Ctx  *context.Context
	Conn *pgx.Conn
}

type Config struct {
	User     string `json:"user"`
	Port     string `json:"port"`
	Password string `json:"password"`
	Addr     string `json:"address"`
	Name     string `json:"database"`
}

func Connect(c Config) (State, error) {
	connUrl := fmt.Sprintf("postgres://%s:%s@%s:%s/%s", c.User, c.Password, c.Addr, c.Port, c.Name)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, connUrl)
	if err != nil {
		return State{}, err
	}
	return State{
		Ctx:  &ctx,
		Conn: conn,
	}, nil
}

func (db *State) QueryGrades(grades []int32) (pgx.Rows, error) {
	return db.Conn.Query(*db.Ctx,
		`SELECT email, first_name, last_name FROM parents WHERE grade && $1`, grades)
}

// returns colums: email, student_first, student_last, grad_year
func (db *State) QueryEnrolledStudents(minYear int) (pgx.Rows, error) {
	return db.Conn.Query(*db.Ctx,
		`SELECT email, student_first, student_last, grad_year FROM enrollment WHERE graduated=false AND NOT (grad_year IS NULL OR grad_year='NaN') AND grad_year>$1 AND email IS NOT NULL;`, minYear)
}

// returns colums: email, student_first, student_last
func (db *State) QueryDepartedStudents(minYear int) (pgx.Rows, error) {
	return db.Conn.Query(*db.Ctx,
		`SELECT email, student_first, student_last FROM enrollment WHERE (graduated=true OR grad_year IS NULL OR grad_year='NaN') AND email IS NOT NULL;`)
}

func (db *State) Close() error {
	return db.Conn.Close(*db.Ctx)
}

func (db *State) InsertParentEmails(t blackbaud.UnorderedTable) error {
	tx, err := db.Conn.BeginTx(*db.Ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	primaryKeys := map[string]bool{
		"email": true,
	}

	// remove all emails to do a full sync
	cmd, err := tx.Exec(*db.Ctx, `TRUNCATE TABLE parents;`)
	if err != nil {
		tx.Rollback(*db.Ctx)
		return fmt.Errorf("failed to truncate parent db: %v, cmd: %s", err, cmd.String())
	}
	// remove null primary keys as not all parents have valid emails in Blackbaud
	removeNull(primaryKeys, t)
	query := fmt.Sprintf(`
	INSERT INTO parents (%s) VALUES (%s)
	ON CONFLICT (%s)
	DO UPDATE SET %s;`,
		strings.Join(t.Columns, ","),
		placeHolders(len(t.Columns)),
		strings.Join(slices.Collect(maps.Keys(primaryKeys)), ","),
		updateAssignments(t.Columns, primaryKeys),
	)
	err = insertRows(db.Ctx, tx, t.Rows, query)
	if err != nil {
		tx.Rollback(*db.Ctx)
		return err
	}
	return tx.Commit(*db.Ctx)
}

func (db *State) InsertAttendance(t blackbaud.UnorderedTable) error {
	tx, err := db.Conn.BeginTx(*db.Ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	primaryKeys := map[string]bool{
		"id": true,
	}

	query := fmt.Sprintf(`
	INSERT INTO attendance (%s) VALUES (%s)
	ON CONFLICT (%s)
	DO NOTHING;`,
		strings.Join(t.Columns, ","),
		placeHolders(len(t.Columns)),
		strings.Join(slices.Collect(maps.Keys(primaryKeys)), ","),
	)
	err = insertRows(db.Ctx, tx, t.Rows, query)
	if err != nil {
		tx.Rollback(*db.Ctx)
		return err
	}
	return tx.Commit(*db.Ctx)
}

// Build SQL placeholders like $1, $2, ..., $N
func placeHolders(count int) string {
	placeholders := make([]string, count)
	for i := range count {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}
	return strings.Join(placeholders, ",")
}

func removeNull(keys map[string]bool, t blackbaud.UnorderedTable) {
	for i := len(t.Rows) - 1; i >= 0; i-- {
		row := t.Rows[i]
		for j, col := range row {
			if keys[t.Columns[j]] && col == nil {
				t.Rows = slices.Delete(t.Rows, i, i+1)
				break
			}
		}
	}
}

func updateAssignments(columns []string, conflicts map[string]bool) string {
	updateAssignments := ""
	for _, col := range columns {
		if conflicts[col] {
			continue // skip conflict key columns
		}
		if updateAssignments != "" {
			updateAssignments += ", "
		}
		updateAssignments += fmt.Sprintf("%s = EXCLUDED.%s", col, col)
	}
	return updateAssignments
}

func (db *State) TranscriptOps(t blackbaud.UnorderedTable, startYear int, endYear int) error {
	tx, err := db.Conn.BeginTx(*db.Ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	cmd, err := transcriptCleanup(db.Ctx, tx, startYear, endYear)
	if err != nil {
		tx.Rollback(*db.Ctx)
		return fmt.Errorf("transcript cleanup failed: %v, cmd: %s", err, cmd)
	}

	// upsert query: https://neon.com/postgresql/postgresql-tutorial/postgresql-upsert
	primaryKeys := map[string]bool{
		"student_user_id": true,
		"term_id":         true,
		"group_id":        true,
		"course_id":       true,
		"grade_id":        true,
	}
	query := fmt.Sprintf(`
	INSERT INTO transcripts (%s) VALUES (%s)
	ON CONFLICT (%s)
	DO UPDATE SET %s;`,
		strings.Join(t.Columns, ","),
		placeHolders(len(t.Columns)),
		strings.Join(slices.Collect(maps.Keys(primaryKeys)), ","),
		updateAssignments(t.Columns, primaryKeys),
	)

	err = insertRows(db.Ctx, tx, t.Rows, query)
	if err != nil {
		tx.Rollback(*db.Ctx)
		return err
	}
	cmd, err = fixNoYearlong(db.Ctx, tx)
	if err != nil {
		tx.Rollback(*db.Ctx)
		return fmt.Errorf("fixing yearlong courses failed: %v, cmd: %s", err, cmd)
	}
	cmd, err = fixNonstandardGrades(db.Ctx, tx)
	if err != nil {
		tx.Rollback(*db.Ctx)
		return fmt.Errorf("unable to fix nonstandard grades: %v, cmd: %s", err, cmd)
	}
	cmd, err = fixFallYearlongs(db.Ctx, tx, startYear, endYear)
	if err != nil {
		tx.Rollback(*db.Ctx)
		return fmt.Errorf("unable to fix fall yearlongs: %v, cmd: %s", err, cmd)
	}
	cmd, err = insertMissingTranscriptCategories(db.Ctx, tx)
	if err != nil {
		tx.Rollback(*db.Ctx)
		return fmt.Errorf("unable to insert missing transcript categories: %v, cmd: %s", err, cmd)
	}
	return tx.Commit(*db.Ctx)
}

func (db *State) EnrollmentOps(enrolled blackbaud.UnorderedTable, departed blackbaud.UnorderedTable) error {
	tx, err := db.Conn.BeginTx(*db.Ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	primaryKeys := map[string]bool{
		"student_user_id": true,
	}
	enrolledInsert := fmt.Sprintf(`
	INSERT INTO enrollment (%s) VALUES (%s)
	ON CONFLICT (%s)
	DO UPDATE SET %s;`,
		strings.Join(enrolled.Columns, ","),
		placeHolders(len(enrolled.Columns)),
		strings.Join(slices.Collect(maps.Keys(primaryKeys)), ","),
		updateAssignments(enrolled.Columns, primaryKeys),
	)
	err = insertRows(db.Ctx, tx, enrolled.Rows, enrolledInsert)
	if err != nil {
		tx.Rollback(*db.Ctx)
		return err
	}
	departedInsert := fmt.Sprintf(`
	INSERT INTO enrollment (%s) VALUES (%s)
	ON CONFLICT (%s)
	DO UPDATE SET %s;`,
		strings.Join(departed.Columns, ","),
		placeHolders(len(departed.Columns)),
		strings.Join(slices.Collect(maps.Keys(primaryKeys)), ","),
		updateAssignments(departed.Columns, primaryKeys),
	)
	err = insertRows(db.Ctx, tx, departed.Rows, departedInsert)
	if err != nil {
		tx.Rollback(*db.Ctx)
		return err
	}
	err = concatGradStatus(db.Ctx, tx)
	if err != nil {
		tx.Rollback(*db.Ctx)
		return err
	}

	return tx.Commit(*db.Ctx)
}

func insertRows(ctx *context.Context, tx pgx.Tx, rows [][]any, insert string) error {
	for _, row := range rows {
		cmd, err := tx.Exec(*ctx, insert, row...)
		if err != nil {
			return fmt.Errorf("db insert failed: %v, cmd: %s, query: %s", err, cmd.String(), insert)
		}
	}
	return nil
}

/*
Updates the 'graduated_status' column in the public.enrollment table based on the values of
'graduated', 'grad_year', and 'depart_date' for each student.

Logic:
  - If `graduated = FALSE`:
  - If `grad_year IS NULL`: status = 'Departed {MM-DD-YYYY}' using depart_date.
  - Else: status = 'Class of {grad_year}'.
  - If `graduated = TRUE`: status = 'Graduated {MM-DD-YYYY}' using depart_date.
  - If depart_date is NULL where it's required in the string, the status will be NULL.
*/
func concatGradStatus(ctx *context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(*ctx, `
    UPDATE public.enrollment
    SET graduated_status = CASE
        WHEN NOT graduated AND grad_year IS NULL AND depart_date IS NOT NULL
            THEN 'Departed ' || TO_CHAR(depart_date::date, 'MM-DD-YYYY')
        WHEN NOT graduated AND grad_year IS NOT NULL
            THEN 'Class of ' || grad_year::text
        WHEN graduated AND depart_date IS NOT NULL
            THEN 'Graduated ' || TO_CHAR(depart_date::date, 'MM-DD-YYYY')
        ELSE NULL
    END;`)
	if err != nil {
		return fmt.Errorf("Error concatting grad status: %v", err)
	}
	return nil
}

func (db *State) TranscriptCommentOps(t blackbaud.UnorderedTable) error {
	tx, err := db.Conn.BeginTx(*db.Ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}

	// upsert query: https://neon.com/postgresql/postgresql-tutorial/postgresql-upsert
	primaryKeys := map[string]bool{
		"student_user_id": true,
	}
	query := fmt.Sprintf(`
	INSERT INTO transcript_comments (%s) VALUES (%s)
	ON CONFLICT (%s)
	DO UPDATE SET %s;`,
		strings.Join(t.Columns, ","),
		placeHolders(len(t.Columns)),
		strings.Join(slices.Collect(maps.Keys(primaryKeys)), ","),
		updateAssignments(t.Columns, primaryKeys),
	)

	err = insertRows(db.Ctx, tx, t.Rows, query)
	if err != nil {
		tx.Rollback(*db.Ctx)
		return err
	}
	return tx.Commit(*db.Ctx)
}

func (db *State) GpaCalculation() error {
	_, err := db.Conn.Exec(*db.Ctx, `
INSERT INTO public.gpa (student_user_id, calculated_gpa)
SELECT 
  student_user_id,
ROUND((SUM(score::NUMERIC * credits::NUMERIC) / NULLIF(SUM(credits::NUMERIC), 0))::NUMERIC, 2)
FROM (
  SELECT
    student_user_id,
    score::NUMERIC,
    CASE 
      WHEN grade_description = 'Year-Long Grades' THEN 2::NUMERIC
      ELSE 1::NUMERIC
    END AS credits
  FROM public.transcripts
  WHERE grade_description NOT IN ('Fall Term Grades YL', 'Spring Term Grades YL')
    AND grade_id != 999999
    AND grade IN ('A', 'A-', 'B+', 'B', 'B-', 'C+', 'C', 'C-', 'D+', 'D', 'D-', 'F', 'WF', 'NC')
) AS weighted
GROUP BY student_user_id
ON CONFLICT (student_user_id)
DO UPDATE SET calculated_gpa = EXCLUDED.calculated_gpa;`)
	if err != nil {
		return err
	}
	return nil
}

// transformation used in the transcript ETL, used for taking yearlong courses with only 1 grade and fixing them to have both grades and be graded for both semesters
func fixNoYearlong(ctx *context.Context, tx pgx.Tx) (string, error) {
	cmd, err := tx.Exec(*ctx, `
		WITH potential_updates AS (
			SELECT student_user_id, school_year, course_id
			FROM public.transcripts
			WHERE grade_description IN ('Fall Term Grades YL', 'Spring Term Grades YL', 'Year-Long Grades')
				OR grade_id = 999999
			GROUP BY student_user_id, school_year, course_id
			HAVING COUNT(*) = 1
		)
		UPDATE public.transcripts t
		SET grade_description = 'no_yearlong_possible',
			grade_id = 888888
		FROM potential_updates p
		WHERE t.student_user_id = p.student_user_id
			AND t.school_year = p.school_year
			AND t.course_id = p.course_id
			AND t.grade_id != 999999
			AND t.grade_description != 'Year-Long Grades';
		`)
	return cmd.String(), err
}

// fixes classes that use credit no credit or "audit", or with specific failing grades
func fixNonstandardGrades(ctx *context.Context, tx pgx.Tx) (string, error) {
	cmd, err := tx.Exec(*ctx, `
WITH transcript_grades AS (
    SELECT
        t.*,
        CASE
            WHEN grade IN ('NC', 'CR', 'I', 'WF', 'WP', 'AU') THEN 'non_letter'
            ELSE 'letter'
        END AS grade_type
    FROM public.transcripts t
    WHERE grade_description IN ('Fall Term Grades YL', 'Spring Term Grades YL', 'Year-Long Grades')
),
paired_grades AS (
    SELECT 
        student_user_id,
        school_year,
        course_id,
        COUNT(*) AS count,
        COUNT(DISTINCT grade_type) AS type_count
    FROM transcript_grades
    GROUP BY student_user_id, school_year, course_id
    HAVING COUNT(*) = 2 AND COUNT(DISTINCT grade_type) = 2
)
UPDATE public.transcripts t
SET grade_description = 'no_yearlong_possible',
    grade_id = 777777
FROM paired_grades p
WHERE t.student_user_id = p.student_user_id
  AND t.school_year = p.school_year
  AND t.course_id = p.course_id
  AND t.grade_description IN ('Fall Term Grades YL', 'Spring Term Grades YL', 'Year-Long Grades');
	`)
	return cmd.String(), err
}

/*
Reassigns grade_id for Fall YL grades when the year is the current year. We need to do this to
allow them to show up in powerBI. Typically Fll YL grades are filtered out because they are overwritten
by YL grades.
*/
func fixFallYearlongs(ctx *context.Context, tx pgx.Tx, startYear int, endYear int) (string, error) {
	yearStr := fmt.Sprintf("%d - %d", startYear, endYear)
	cmd, err := tx.Exec(*ctx, `
		UPDATE public.transcripts
        SET grade_description = 'current_fall_yl',
        grade_id = 666666
        WHERE (school_year = $1 AND
        grade_description = 'Fall Term Grades YL');`,
		yearStr,
	)
	return cmd.String(), err
}

/*
This function tx *pgx.Tx, removes records with grade_id = 999999, 888888, 777777, 666666 and restores Fall YL grades.
This is done to prevent duplicates on a reimport because the grade_id is part of the primary key.
*/
func transcriptCleanup(ctx *context.Context, tx pgx.Tx, startYear int, endYear int) (string, error) {
	// List of the the last 4 academic years
	yearList := []int{}
	for i := range 5 {
		yearList = append(yearList, endYear-i)
	}
	transcript_query := `
                DELETE FROM public.transcripts
                                     WHERE (grade_id = 888888
                                     OR grade_id = 777777
                                     OR grade_id = 666666
                                     OR grade_description = 'Senior Mid-Term Grades')
                                     AND school_year = $1
	`
	for _, year := range yearList {
		cmd, err := tx.Exec(*ctx, transcript_query, fmt.Sprintf("%d - %d", year, year+1))
		if err != nil {
			return cmd.String(), err
		}
	}

	restoreFallYlQuery := `
UPDATE public.transcripts
    SET grade_description = 'Fall Term Grades YL', grade_id = 2154180
    WHERE (school_year != $1 
    AND grade_id = 666666);
	`
	cmd, err := tx.Exec(*ctx, restoreFallYlQuery, fmt.Sprintf("%d - %d", startYear, endYear))
	if err != nil {
		return cmd.String(), err
	}
	deleteScheduledCoursesQuery := `
DELETE FROM public.transcripts
    WHERE grade_id = 999999
	`
	cmd, err = tx.Exec(*ctx, deleteScheduledCoursesQuery)
	return cmd.String(), err
}

/*
This function tx *pgx.Tx, will insert transcript categories where none exist.
This is always the case for grade_id = 999999 as they do not exist for scheduled courses.
The transcript categories are identified based on the course prefix, which is the first
word in the course code. The transcript category mappings are stored in the public.course_codes table, which needs to
be manually kept up to date until we can get a better solution.
*/
func insertMissingTranscriptCategories(ctx *context.Context, tx pgx.Tx) (string, error) {
	cmd, err := tx.Exec(*ctx, `
        WITH ranked_prefixes AS (
            SELECT
                transcripts.course_code,
                course_codes.transcript_category,
                ROW_NUMBER() OVER (PARTITION BY transcripts.course_code ORDER BY LENGTH(course_codes.course_prefix) DESC) AS rn
            FROM
                public.transcripts
            JOIN
                public.course_codes
            ON
                transcripts.course_code::text LIKE course_codes.course_prefix || '%'
            WHERE
                transcripts.transcript_category = 'NaN'
        )
        UPDATE public.transcripts
        SET transcript_category = ranked_prefixes.transcript_category
        FROM ranked_prefixes
        WHERE public.transcripts.course_code = ranked_prefixes.course_code
        AND public.transcripts.transcript_category = 'NaN'
        AND ranked_prefixes.rn = 1;
	`)
	return cmd.String(), err
}

// end transcript helpers
