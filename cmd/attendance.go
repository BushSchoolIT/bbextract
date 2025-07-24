package cmd

import (
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/BushSchoolIT/extractor/blackbaud"
	"github.com/BushSchoolIT/extractor/database"
	"github.com/spf13/cobra"
)

func Attendance(cmd *cobra.Command, args []string) error {
	// load config and blackbaud API
	api, err := blackbaud.NewBBApiConnector(fAuthFile)
	if err != nil {
		slog.Error("Unable to access blackbaud api", slog.Any("error", err))
		return err
	}
	config, err := loadConfig(fConfigFile)
	if err != nil {
		slog.Error("Unable to load config", slog.Any("error", err))
		return err
	}
	db, err := database.Connect(config.Postgres)
	if err != nil {
		slog.Error("Unable to connect to DB", slog.Any("error", err))
		return err
	}
	defer db.Close()

	t := blackbaud.UnorderedTable{}
	for _, id := range config.Attendance.LevelIDs {
		req, err := api.NewRequest(http.MethodGet, blackbaud.AttendanceApi, http.NoBody)
		q := req.URL.Query()
		q.Add("level_id", id)
		q.Add("day", time.Now().Format("01/02/2006")) // DD/MM/YYYY
		q.Add("offering_type", "1")
		req.URL.RawQuery = q.Encode()
		resp, err := api.Client.Do(req)
		if err != nil {
			slog.Error("Unable to get attendance data", slog.String("id", id))
			return err
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			slog.Error("Unable to get read response body", slog.String("id", id))
			return err
		}
		if resp.StatusCode != http.StatusOK {
			slog.Error("Response returned unexpected status code", slog.String("id", id), slog.Int("code", resp.StatusCode), slog.String("body", string(body)))
			return err
		}
		parsed := blackbaud.Attendance{}
		err = json.Unmarshal(body, &parsed)
		if err != nil {
			slog.Error("Unable to unmarshal attendance data", slog.String("id", id))
			return err
		}
		for _, row := range parsed.Value {
			if len(t.Columns) == 0 {
				t.Columns = slices.Collect(maps.Keys(row))
			}

			newRow := []any{}
			for _, col := range t.Columns {
				newRow = append(newRow, row[col])
			}
			t.Rows = append(t.Rows, newRow)
		}
	}
	err = db.InsertAttendance(t)
	if err != nil {
		slog.Error("Unable to insert emails", slog.Any("error", err))
		return err
	}
	return nil
}
