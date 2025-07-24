package cmd

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"

	"github.com/BushSchoolIT/extractor/database"
	"github.com/spf13/cobra"
)

var (
	rootCmd = &cobra.Command{
		SilenceErrors: true,
		SilenceUsage:  true,
		Use:           "bbextract",
		Short:         "bbextract is the successor to BlackBaudExtractor rewritten in Go",
	}
	transcriptCmd = &cobra.Command{
		Use:   "transcripts",
		Short: "Extracts transcript info from blackbaud and imports it into the database",
		Run:   Transcripts,
	}
	gpaCmd = &cobra.Command{
		Use:   "gpa",
		Short: "Runs GPA ETL independently",
		RunE:  Gpa,
	}
	gsyncStudentsCmd = &cobra.Command{
		Use:   "gsync-students",
		Short: "Synchronizes the google logins of students with the database in the data warehouse",
		RunE:  GSyncStudents,
	}
	commentsCmd = &cobra.Command{
		Use:   "comments",
		Short: "Extracts transcript comments from blackbaud and imports it into the database",
		RunE:  Comments,
	}
	parentsCmd = &cobra.Command{
		Use:   "parents",
		Short: "Extracts parent info from blackbaud and imports it into the database for mailing info",
		RunE:  Parents,
	}
	attendanceCmd = &cobra.Command{
		Use:   "attendance",
		Short: "Extracts attendance info from blackbaud and imports it into the database",
		RunE:  Attendance,
	}
	enrollmentCmd = &cobra.Command{
		Use:   "enrollment",
		Short: "Extracts enrollment info from blackbaud and imports into the database",
		RunE:  Enrollment,
	}
	fLogFile    string
	fLogLevel   string
	fConfigFile string
	fAuthFile   string
)

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		slog.Error("An Error Occurred", slog.Any("error", err))
		os.Exit(1)
	}
}

func init() {
	rootCmd.AddCommand(transcriptCmd)
	rootCmd.AddCommand(parentsCmd)
	rootCmd.AddCommand(attendanceCmd)
	rootCmd.AddCommand(commentsCmd)
	rootCmd.AddCommand(gpaCmd)
	rootCmd.AddCommand(gsyncStudentsCmd)
	rootCmd.AddCommand(enrollmentCmd)
	rootCmd.PersistentFlags().StringVar(&fConfigFile, "config", "config.json", "config file containing list IDs")
	rootCmd.PersistentFlags().StringVar(&fAuthFile, "auth", "bb_auth.json", "authconfig for blackbaud")
}

type Config struct {
	ParentsID            string          `json:"parents_list_id"`
	TranscriptListIDs    []string        `json:"transcript_list_ids"`
	Postgres             database.Config `json:"postgres"`
	TranscriptCommentsID string          `json:"transcript_comments_id"`
	Attendance           struct {
		LevelIDs []string `json:"level_ids"`
	} `json:"attendance"`
	EnrollmentListIDs struct {
		Departed string `json:"departed"`
		Enrolled string `json:"enrolled"`
	} `json:"enrollment_list_ids"`
	Google struct {
		OUStudentsPath string `json:"ou_students_path"`
		OUStudentFmt   string `json:"ou_student_fmt"`
		AdminEmail     string `json:"admin_email"`
	} `json:"google"`
}

func loadConfig(configPath string) (Config, error) {
	var config Config
	f, err := os.Open(configPath)
	if err != nil {
		return config, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return config, err
	}
	err = json.Unmarshal(data, &config)
	if err != nil {
		return config, err
	}
	return config, nil
}
