package blackbaud

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"time"

	"golang.org/x/time/rate"
)

type SkyAppInfo struct {
	AppID     string `json:"app_id"`
	AppSecret string `json:"app_secret"`
}

type TokenInfo struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

type OtherInfo struct {
	ApiSubscriptionKey string `json:"api_subscription_key"`
	TestApiEndpoint    string `json:"test_api_endpoint"`
	RedirectUri        string `json:"redirect_uri"`
}

type Config struct {
	Other             OtherInfo  `json:"other"`
	Tokens            TokenInfo  `json:"tokens"`
	SkyAppInformation SkyAppInfo `json:"sky_app_information"`
}

type BBAPIConnector struct {
	config     *Config
	configPath string
	ctx        context.Context
	limiter    *rate.Limiter
	Client     *http.Client
	EndYear    int
	StartYear  int
}

type Column struct {
	Name  string `json:"name"`
	Value any    `json:"value"`
}

type Row struct {
	Columns []Column `json:"columns"`
}

type AdvancedList struct {
	Results struct {
		Rows []Row `json:"rows"`
	} `json:"results"`
	NextLink string `json:"next_link"`
	Paging   struct {
		RemainingRows int `json:"remaining_rows"`
		Page          int `json:"page"`
		PageSize      int `json:"page_size"`
		TotalRows     int `json:"total_rows"`
	} `json:"paging"`
}

type Attendance struct {
	Value []map[string]any `json:"value"`
}

type UserReadCollection struct {
	Count    int32      `json:"count"`
	NextLink string     `json:"next_link"`
	Value    []UserRead `json:"value"`
}

type UserRead struct {
	ID            int32  `json:"id"`
	FirstName     string `json:"first_name"`
	PreferredName string `json:"preferred_name"`
	MiddleName    string `json:"middle_name"`
	LastName      string `json:"last_name"`
	Email         string `json:"email"`
	Display       string `json:"display"`
}

type Parent struct {
	FirstName string
	LastName  string
	Grades    []int
}

const (
	TokenUrl        string = "https://oauth2.sky.blackbaud.com/token"
	ListsApi        string = "https://api.sky.blackbaud.com/school/v1/lists/advanced"
	Host            string = "api.sky.blackbaud.com"
	YearApi         string = "https://api.sky.blackbaud.com/school/v1/years"
	AttendanceApi   string = "https://api.sky.blackbaud.com/school/v1/attendance"
	UsersApi        string = "https://api.sky.blackbaud.com/school/v1/users"
	RedirectUri     string = "http://localhost:13631/callback"
	TestApiEntpoint string = "https://api.sky.blackbaud.com/school/v1/roles"
)

// Create a new API connector using an existing JSON path (MUST exist, currently we don't generate the auth stuff just yet)
// if the auth token is bad, it refreshes it
func NewBBApiConnector(configPath string) (*BBAPIConnector, error) {
	config, err := loadConfig(configPath)
	if err != nil {
		return nil, err
	}

	client := &http.Client{}
	connector := &BBAPIConnector{
		&config,
		configPath,
		context.Background(),
		rate.NewLimiter(rate.Every(250*time.Millisecond), 1),
		client,
		0,
		0,
	}
	req, err := connector.NewRequest(http.MethodGet, config.Other.TestApiEndpoint, http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := connector.Client.Do(req)
	if err != nil {
		return nil, err
	}
	body, _ := io.ReadAll(resp.Body)
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		err = refreshToken(&config)
		if err != nil {
			return nil, err
		}
		err = saveConfig(configPath, config)
		if err != nil {
			return nil, err
		}

		return NewBBApiConnector(configPath)
	case http.StatusOK:
		start, end, err := getYears(connector)
		if err != nil {
			return nil, err
		}
		// set them properly if the year is correct
		connector.StartYear = start
		connector.EndYear = end
		return connector, nil
	}

	return nil, fmt.Errorf("unexpected response: %d, response body: %s, %v", resp.StatusCode, string(body), resp)

}

func refreshToken(config *Config) error {

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", config.Tokens.RefreshToken)
	form.Set("preserve_refresh_token", "true")
	form.Set("client_id", config.SkyAppInformation.AppID)
	form.Set("client_secret", config.SkyAppInformation.AppSecret)
	resp, err := http.PostForm(TokenUrl, form)
	if err != nil {
		return err
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("could not refresh auth token, response code returned: %d, body: %s", resp.StatusCode, string(body))
	}

	tokenResp := struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}{}

	err = json.Unmarshal(body, &tokenResp)
	if err != nil {
		return err
	}

	config.Tokens.AccessToken = tokenResp.AccessToken
	config.Tokens.RefreshToken = tokenResp.RefreshToken
	return resp.Body.Close()
}

func (b *BBAPIConnector) GetAdvancedList(id string, page int) (AdvancedList, error) {
	req, err := b.NewRequest(http.MethodGet, AdvancedListApi(id, page), http.NoBody)
	if err != nil {
		return AdvancedList{}, fmt.Errorf("Unable to create request: %v", err)
	}
	resp, err := b.Client.Do(req)
	if err != nil {
		return AdvancedList{}, fmt.Errorf("Unable to access blackbaud api: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return AdvancedList{}, fmt.Errorf("Blackbaud API returned unexpected status code, code: %d, body: %s", resp.StatusCode, string(body))
	}

	var parsed AdvancedList
	if err := json.Unmarshal(body, &parsed); err != nil {
		return AdvancedList{}, fmt.Errorf("JSON unmarshal failed: %v", err)
	}
	return parsed, resp.Body.Close()
}

func (b *BBAPIConnector) NewRequest(method string, url string, body io.Reader) (*http.Request, error) {
	err := b.limiter.Wait(b.ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(method, url, body)

	if err != nil {
		return req, err
	}

	req.Header.Set("Bb-Api-Subscription-Key", b.config.Other.ApiSubscriptionKey)
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", b.config.Tokens.AccessToken))
	req.Header.Set("Host", Host)
	req.Header.Set("Content-Type", "application/json")
	return req, nil
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

func saveConfig(configPath string, config Config) error {
	const filePerm = 0644

	data, err := json.MarshalIndent(config, "", "    ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	// Write file with permissions — respected on Unix, ignored on Windows
	err = os.WriteFile(configPath, data, filePerm)
	if err != nil {
		return fmt.Errorf("failed to write config to %s: %w", configPath, err)
	}

	return nil
}

/* get the academic year from blackbaud */
func getYears(connector *BBAPIConnector) (int, int, error) {
	req, err := connector.NewRequest(http.MethodGet, YearApi, http.NoBody)
	if err != nil {
		return 0, 0, err
	}
	parsed := struct {
		Value []struct {
			CurrentYear     bool   `json:"current_year"`
			SchoolYearLabel string `json:"school_year_label"`
			BeginDate       string `json:"begin_date"`
			EndDate         string `json:"end_date"`
		} `json:"value"`
	}{}
	resp, err := connector.Client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	body, err := io.ReadAll(resp.Body)
	err = json.Unmarshal(body, &parsed)
	if err != nil {
		return 0, 0, err
	}

	yearID := -1
	for i, year := range parsed.Value {
		if year.CurrentYear {
			yearID = i
			break
		}
	}
	// if empty, cry about it
	if yearID == -1 {
		return 0, 0, fmt.Errorf("Unabled to find current year")
	}

	beginTime, err := time.Parse(time.RFC3339, parsed.Value[yearID].BeginDate)
	if err != nil {
		return 0, 0, fmt.Errorf("Unable to parse start year time: %v", err)
	}

	endTime, err := time.Parse(time.RFC3339, parsed.Value[yearID].EndDate)
	if err != nil {
		return 0, 0, fmt.Errorf("Unabled to end year time: %v", err)
	}

	return beginTime.Year(), endTime.Year(), resp.Body.Close()
}

func AdvancedListApi(id string, page int) string {
	return fmt.Sprintf("%s/%s?page=%d", ListsApi, id, page)
}

// Thin Datastructure used for processing and inserting into the DB :)
type UnorderedTable struct {
	Columns []string
	Rows    [][]any
}

func ProcessList(api *BBAPIConnector, id string) (UnorderedTable, error) {
	t := UnorderedTable{}
	for page := 1; ; page++ {
		parsed, err := api.GetAdvancedList(id, page)
		if err != nil {
			slog.Error("Unable to get advanced list", slog.String("id", id), slog.Int("page", page))
			return t, fmt.Errorf("Unable to get advanced list, id: %s, err: %v", id, err)
		}
		if len(parsed.Results.Rows) == 0 {
			break // No more data
		}
		if len(t.Columns) == 0 {
			t.Columns = GetColumns(parsed.Results.Rows[0])
		}

		slog.Info("Collecting Data From Page", slog.Int("page", page), slog.String("id", id))
		for _, row := range parsed.Results.Rows {
			newRow := []any{}
			for _, col := range row.Columns {
				newRow = append(newRow, col.Value)
			}
			t.Rows = append(t.Rows, newRow)
		}
	}
	return t, nil
}

func GetColumns(row Row) []string {
	columns := []string{}
	for _, col := range row.Columns {
		columns = append(columns, col.Name)
	}
	return columns
}
