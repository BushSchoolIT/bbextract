package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"mime"
	"path"
	"strings"

	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httputil"
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
	GDirectoryUser  string = "https://admin.googleapis.com/admin/directory/v1/users"
	MaxStudents     int    = 100000
)

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

	data, err := os.ReadFile("g_auth.json")
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

	ouMap := map[string]*admin.OrgUnit{}
	ouListCall := gSrv.Orgunits.List(GCustomer).Type("all")
	ouList, err := ouListCall.Do()
	if err != nil {
		return err
	}
	for _, ou := range ouList.OrganizationUnits {
		ouMap[ou.OrgUnitPath] = ou
	}
	userMap := make(map[string]bool, MaxStudents)
	userCall := gSrv.Users.List().Customer(GCustomer)
	err = userCall.Pages(ctx, func(users *admin.Users) error {
		for _, u := range users.Users {
			lowerCaseEmail := strings.ToLower(u.PrimaryEmail)
			if strings.Contains(u.OrgUnitPath, "Students") {
				userMap[lowerCaseEmail] = true
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	emailReqMap := map[string]*http.Request{}
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
				GivenName:  studentFirst,
				FamilyName: studentLast,
			},
			Suspended: false,
			// Password is kinda dumb since students login using SSO 🤷 it's just required by the API
			Password:    "DefaultStudentPassword",
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
		emailReqMap[email] = req
	}
	addReq, err := BatchRequest(emailReqMap, http.MethodPost, GDirectoryBatch)
	if err != nil {
		return err
	}
	dump, _ := httputil.DumpRequest(addReq, true)
	fmt.Printf("dump:\n%s\n", dump)
	resp, err := client.Do(addReq)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bad status code returned: %d, status: %s", resp.StatusCode, resp.Status)
	}

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
		email := strings.TrimPrefix(headers.Get("Content-Id"), "response-")
		req, exists := emailReqMap[email]
		if !exists {
			return fmt.Errorf("unable to find email in emailMap: %s", email)
		}
		partResp, err := http.ReadResponse(r, req)
		if err != nil {
			return fmt.Errorf("unable to read multipart response: %v", err)
		}
		defer partResp.Body.Close()
		if partResp.StatusCode == http.StatusOK {
			continue
		}
		// unhandled error
		b, _ := io.ReadAll(partResp.Body)
		return fmt.Errorf("bad request, status code: %d, status: %v, body: %s", partResp.StatusCode, partResp.Status, b)
	}

	return nil
}

// https://developers.google.com/workspace/admin/directory/v1/guides/manage-users
// we make our own structs here because the google API package sucks and we can't access the raw requests
type GName struct {
	GivenName  string `json:"givenName"`
	FamilyName string `json:"familyName"`
}
type GUser struct {
	Email       string `json:"primaryEmail"`
	Name        GName  `json:"name"`
	Suspended   bool   `json:"suspended"`
	Password    string `json:"password"`
	OrgUnitPath string `json:"orgUnitPath,omitempty"`
}

// Creates a batch request with the provided map, key corresponds to the content-id and creates a request
func BatchRequest(reqMap map[string]*http.Request, method string, path string) (*http.Request, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for contentId, req := range reqMap {
		w, err := writer.CreatePart(textproto.MIMEHeader{
			"Content-Type": {"application/http"},
			"Content-ID":   {contentId},
		})
		if err != nil {
			return nil, err
		}
		if err = req.Write(w); err != nil {
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
