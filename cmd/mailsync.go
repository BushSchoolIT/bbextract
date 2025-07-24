package cmd

import (
	"fmt"
	"log"
	"log/slog"

	"github.com/BushSchoolIT/extractor/database"
	"github.com/BushSchoolIT/extractor/octopus"
	"github.com/spf13/cobra"
)

func Mailsync(cmd *cobra.Command, args []string) error {
	// load config and blackbaud API
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

	mailconfig, err := octopus.LoadMailInfo(fMailInfoFile)
	if err != nil {
		return fmt.Errorf("can't load data %v", err)
	}
	emailOctopusAPIKey, err := octopus.GetApiKey(fOctoAuthFile)
	if err != nil {
		return err
	}

	for _, info := range mailconfig {
		slog.Info("Processing Grades", slog.String("name", info.Name), slog.Any("grades", info.Grades), slog.String("list_id", info.ID))
		listInfo, err := octopus.GetListInfo(emailOctopusAPIKey, info.ID)
		if err != nil {
			return fmt.Errorf("unable to get list info: %v", err)
		}

		rows, err := db.QueryGrades(info.Grades)
		if err != nil {
			return fmt.Errorf("Unable to query grades: %v", err)
		}
		emails, err := octopus.GetEmails(emailOctopusAPIKey, info.ID, listInfo)
		if err != nil {
			return fmt.Errorf("Unable to get emails: %v", err)
		}

		upsertList, deleteList, err := octopus.GetLists(emails, rows)

		log.Printf("Adding emails: %v", upsertList)
		octopus.SubscribeEmails(emailOctopusAPIKey, info.ID, upsertList)
		log.Printf("Deleting emails: %v", deleteList)
		octopus.DeleteEmails(emailOctopusAPIKey, info.ID, deleteList)
		rows.Close()
	}

	return nil
}
