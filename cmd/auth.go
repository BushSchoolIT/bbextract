package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/BushSchoolIT/extractor/blackbaud"
	"github.com/BushSchoolIT/extractor/octopus"
	"github.com/spf13/cobra"
)

func saveJson(path string, v any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	buf, err := json.MarshalIndent(v, "", "    ")
	if err != nil {
		return err
	}
	_, err = f.Write(buf)
	if err != nil {
		return err
	}
	return f.Close()
}

func userInput(r io.Reader) (string, error) {
	scanner := bufio.NewScanner(r)
	if scanner.Scan() {
		input := scanner.Text()
		input = strings.TrimSpace(input)
		return input, nil
	}
	return "", scanner.Err()
}

func GenerateAuthFiles(cmd *cobra.Command, args []string) error {
	fmt.Printf(`
Generating authfiles...
Enter your Email Octopus authkey
key: `)
	key, err := userInput(os.Stdin)
	octo := octopus.Auth{
		Key: key,
	}
	err = saveJson(fOctoAuthFile, octo)
	if err != nil {
		return err
	}
	fmt.Printf(`
Enter your Blackbaud App/Client ID
id: `)
	appId, err := userInput(os.Stdin)
	if err != nil {
		return err
	}
	fmt.Printf(`
Enter your Blackbaud API Subscription Key
key: `)
	apiSubscriptionKey, err := userInput(os.Stdin)
	if err != nil {
		return err
	}
	fmt.Printf(`
Enter your Blackbaud App/Client Secret
id: `)
	secret, err := userInput(os.Stdin)
	if err != nil {
		return err
	}
	fmt.Printf(`
In order to complete generating your auth code, visit the site: https://oauth2.sky.blackbaud.com/authorization?client_id=%s&response_type=code&redirect_uri=%s
		`, appId, blackbaud.RedirectUri)

	ret := make(chan string)
	server := &http.Server{Addr: ":13631"}
	http.HandleFunc("/callback", createHandleCallback(server, ret))
	go func() {
		err = server.ListenAndServe()
		if err != nil {
			return
		}
	}()
	authCode := ""
	for v := range ret {
		authCode = v
	}
	if authCode == "" {
		return fmt.Errorf("auth code empty")
	}
	fmt.Printf("Authorization code received: %s\n", authCode)
	data := url.Values{}
	data.Set("grant_type", "authorization_code")
	data.Set("redirect_uri", blackbaud.RedirectUri)
	data.Set("code", authCode)
	data.Set("client_id", appId)
	data.Set("client_secret", secret)
	resp, err := http.PostForm(blackbaud.TokenUrl, data)
	body, err := io.ReadAll(resp.Body)

	if err != nil {
		return err
	}
	v := TokenResponse{}
	err = json.Unmarshal(body, &v)
	if err != nil {
		return err
	}
	config := blackbaud.Config{}
	config.Other.ApiSubscriptionKey = apiSubscriptionKey
	config.Other.TestApiEndpoint = blackbaud.TestApiEntpoint
	config.Other.RedirectUri = blackbaud.RedirectUri
	config.Tokens.AccessToken = v.AccessToken
	config.Tokens.RefreshToken = v.RefreshToken
	config.SkyAppInformation.AppID = appId
	config.SkyAppInformation.AppSecret = secret
	saveJson(fAuthFile, config)

	return nil
}

type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
}

func createHandleCallback(server *http.Server, ret chan string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authCode := r.URL.Query().Get("code")
		if authCode != "" {
			w.Write([]byte("Authorization code received. You can close this window now."))

			ret <- authCode
			close(ret)
			server.Shutdown(context.Background())
		} else {
			http.Error(w, "Authorization code not found", http.StatusBadRequest)
		}
	}
}
