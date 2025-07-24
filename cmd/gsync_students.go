package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"mime"
	"path"
	"slices"
	"strings"
	"time"

	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"

	"github.com/BushSchoolIT/extractor/blackbaud"
	"github.com/BushSchoolIT/extractor/database"
	"github.com/spf13/cobra"
	"golang.org/x/oauth2/google"

	admin "google.golang.org/api/admin/directory/v1"
	"google.golang.org/api/option"
)

const (
	GCustomer       string = "my_customer"
	GUserScope      string = "https://www.googleapis.com/auth/admin.directory.user"
	GOrgUnitScope   string = "https://www.googleapis.com/auth/admin.directory.orgunit"
	GDirectoryBatch string = "https://admin.googleapis.com/batch/admin/directory/v1"
	GDirectoryUser  string = "https://admin.googleapis.com/admin/directory/v1/users/"
	MaxStudents     int    = 10000
)

// https://developers.google.com/workspace/admin/directory/v1/guides/manage-users
// we make our own structs here because the google API package sucks and we can't access the raw requests
type GName struct {
	GivenName  string `json:"givenName,omitempty"`
	FamilyName string `json:"familyName,omitempty"`
}
type GUser struct {
	Email       string `json:"primaryEmail,omitempty"`
	Name        GName  `json:"name"`
	Suspended   bool   `json:"suspended,omitempty"`
	Password    string `json:"password,omitempty"`
	OrgUnitPath string `json:"orgUnitPath,omitempty"`
}

// used for multipart requests
type Subrequest struct {
	ContentId string
	Req       *http.Request
}

func GSyncStudents(cmd *cobra.Command, args []string) error {
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

	data, err := os.ReadFile(fGAuthFile)
	if err != nil {
		return fmt.Errorf("unable to read service account file: %v", err)
	}

	jwt, err := google.JWTConfigFromJSON(data,
		GUserScope,
		GOrgUnitScope)
	if err != nil {
		return fmt.Errorf("unable to parse service account key: %v", err)
	}
	// Manually set email to admin because that's normal??? <Citation needed>
	jwt.Subject = config.Google.AdminEmail
	ctx := context.Background()
	client := jwt.Client(context.Background())
	gSrv, err := admin.NewService(ctx, option.WithTokenSource(jwt.TokenSource(ctx)))
	if err != nil {
		return err
	}

	slog.Info("Getting OU info from Google")
	ouMap := map[string]*admin.OrgUnit{}
	ouListCall := gSrv.Orgunits.List(GCustomer).OrgUnitPath(config.Google.OUStudentsPath)
	ouList, err := ouListCall.Do()
	if err != nil {
		return err
	}
	for _, ou := range ouList.OrganizationUnits {
		ouMap[ou.OrgUnitPath] = ou
	}
	slog.Info("Finished collecting OU info from Google")
	// the boolean whether or not the user is suspended
	slog.Info("Collecting student info from Google")
	userMap := make(map[string]bool, MaxStudents)
	userCall := gSrv.Users.List().Customer(GCustomer).Query("orgUnitPath=" + config.Google.OUStudentsPath)
	err = userCall.Pages(ctx, func(users *admin.Users) error {
		for _, u := range users.Users {
			lowerCaseEmail := strings.ToLower(u.PrimaryEmail)
			userMap[lowerCaseEmail] = u.Suspended
		}
		slog.Info("Students processed from google", slog.Any("processed", len(users.Users)))
		return nil
	})
	if err != nil {
		return err
	}
	slog.Info("Finished collecting student info from Google")

	slog.Info("Collecting student info from DB")
	emailReqs := []Subrequest{}
	enrolled, err := db.QueryEnrolledStudents(api.StartYear - 1)
	if err != nil {
		return err
	}
	defer enrolled.Close()
	for enrolled.Next() {
		var email, studentFirst, studentLast, gradYear string
		err := enrolled.Scan(&email, &studentFirst, &studentLast, &gradYear)
		if err != nil {
			return err
		}
		email = strings.ToLower(email)
		_, exists := userMap[email]
		if exists {
			continue
		}
		user := GUser{
			Email: email,
			Name: GName{
				studentFirst,
				studentLast,
			},
			Suspended: false,
			// Password is kinda dumb since students login using SSO 🤷 it's just required by the API
			Password: "DefaultStudentPassword",
			// /Students/Class of <grad_year>
			OrgUnitPath: config.Google.OUStudentsPath + config.Google.OUStudentFmt + gradYear,
		}
		data, err := json.Marshal(user)
		if err != nil {
			return err
		}
		req, err := http.NewRequest(http.MethodPost, GDirectoryUser, bytes.NewReader(data))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")

		_, exists = ouMap[user.OrgUnitPath]
		if !exists {
			// create the ou directly (outside the batch request), not guarunteed to run in order
			slog.Info("ou doesn't exist, creating... %v", slog.String("path", user.OrgUnitPath), slog.Any("map", ouMap))
			name := path.Base(user.OrgUnitPath)
			parent := path.Dir(user.OrgUnitPath)
			unit := admin.OrgUnit{
				Name:              name,
				ParentOrgUnitPath: parent,
			}
			call := gSrv.Orgunits.Insert(GCustomer, &unit)
			newUnit, err := call.Do()
			// can't add OU? Skill issue I guess
			if err != nil {
				return fmt.Errorf("unable to create org unit: %v, returned: %v", unit, err)
			}
			ouMap[newUnit.OrgUnitPath] = newUnit
		}
		emailReqs = append(emailReqs, Subrequest{email, req})
	}
	slog.Info("Finished enrolled students")

	departed, err := db.QueryDepartedStudents(api.StartYear - 1)
	if err != nil {
		return err
	}
	defer departed.Close()
	for departed.Next() {
		var email, studentFirst, studentLast string
		err := departed.Scan(&email, &studentFirst, &studentLast)
		if err != nil {
			return err
		}
		email = strings.ToLower(email)
		suspended, exists := userMap[email]
		// user does not exist, do not attempt to update
		if !exists {
			continue
		}
		// user already suspended, skip
		if suspended {
			continue
		}
		u := GUser{
			Name: GName{
				studentFirst,
				studentLast,
			},
			Suspended: true,
		}
		data, err := json.Marshal(u)
		if err != nil {
			return err
		}
		req, err := http.NewRequest(http.MethodPut, GDirectoryUser+email, bytes.NewReader(data))
		if err != nil {
			return err
		}
		emailReqs = append(emailReqs, Subrequest{email, req})
	}
	slog.Info("Finished departed students")

	count := 0
	slog.Info("Finished ETL", slog.Int("total_to_be_processed", len(emailReqs)))
	for c := range slices.Chunk(emailReqs, 50) {
		slog.Info("Processing more students", slog.Int("to_be_processed", len(c)), slog.Int("total", len(emailReqs)), slog.Int("processed", count))
		addReq, err := BatchRequest(c, http.MethodPost, GDirectoryBatch)
		if err != nil {
			return err
		}
		resp, err := client.Do(addReq)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("bad status code returned: %d, status: %s", resp.StatusCode, resp.Status)
		}
		err = ProcessBatchResponse(resp)
		if err != nil {
			return err
		}
		// Rate limit for a minute (no more than 60 requests per minute)
		count = min(count+50, len(emailReqs))
		slog.Info("Finished slice", slog.Int("processed", count), slog.Int("total", len(emailReqs)), slog.Int("chunk_processed", len(c)))
		time.Sleep(time.Minute)
	}

	return nil
}

/*
Creates a batch request with the provided map, key corresponds to the content-id and creates a request
docs here: https://developers.google.com/workspace/admin/directory/v1/guides/batch
*/
func BatchRequest(reqs []Subrequest, method string, path string) (*http.Request, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, subReq := range reqs {
		w, err := writer.CreatePart(textproto.MIMEHeader{
			"Content-Type": {"application/http"},
			"Content-ID":   {subReq.ContentId},
		})
		if err != nil {
			return nil, err
		}
		if err = subReq.Req.Write(w); err != nil {
			return nil, err
		}
	}
	writer.Close()
	req, err := http.NewRequest(method, path, &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "multipart/mixed; boundary="+writer.Boundary())
	return req, nil
}

func ProcessBatchResponse(resp *http.Response) error {
	mediaType, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return fmt.Errorf("invalid content type: %v", err)
	}
	if mediaType != "multipart/mixed" {
		return fmt.Errorf("wrong mediatype in response: %s, should be multipart", mediaType)
	}
	respBoundary, exists := params["boundary"]
	if !exists {
		return fmt.Errorf("unable to get boundary in multipart response")
	}
	mr := multipart.NewReader(resp.Body, respBoundary)
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			// no more stuff to read
			break
		}
		if err != nil {
			return fmt.Errorf("unable get multipart response: %v", err)
		}
		headers := part.Header
		r := bufio.NewReader(part)
		contentId := strings.TrimPrefix(headers.Get("Content-Id"), "response-")
		partResp, err := http.ReadResponse(r, nil)
		if err != nil {
			return fmt.Errorf("unable to read multipart response: %v", err)
		}
		defer partResp.Body.Close()
		if partResp.StatusCode == http.StatusOK {
			continue
		}
		// unhandled error
		b, _ := io.ReadAll(partResp.Body)
		return fmt.Errorf("bad request, status code: %d, status: %v, body: %s, contentId: %s", partResp.StatusCode, partResp.Status, b, contentId)
	}
	return nil
}
