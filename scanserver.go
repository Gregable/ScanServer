package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"code.google.com/p/goauth2/oauth"
	drive "code.google.com/p/google-api-go-client/drive/v2"
)

// Flags
var config_file = flag.String(
	"config-file",
	"/home/greg/config-file",
	"File containing ScanServer configuration code")

var oauth_config = &oauth.Config{
	ClientId:     "",
	ClientSecret: "",
	Scope:        drive.DriveScope,
	AuthURL:      "https://accounts.google.com/o/oauth2/auth",
	TokenURL:     "https://accounts.google.com/o/oauth2/token",
}

func main() {
	flag.Parse()

	config := ParseConfig(*config_file)
	if !PromptIfMissingConfigFields(&config) {
		return
	}

	to_upload_chan := make(chan string)
	done_upload_chan := make(chan string)
	go PeriodicallyListScans(config, to_upload_chan)
	go UploadFiles(config, to_upload_chan, done_upload_chan)
	for filename := range done_upload_chan {
		fmt.Println("Successfully Uploaded file ", filename)
	}
}

// Returns true if config is fully specified.
func PromptIfMissingConfigFields(config *ScanServerConfig) bool {
	// User must provide app client id and secret in config
	if config.ClientId == "" || config.ClientSecret == "" {
		fmt.Println(
			"ClientId and ClientSecret must be configured in config_file.\n",
			"See README for information on how to generate these.")
		// We write a config here so there is a template available to the user.
		WriteConfig(*config_file, *config)
		return false
	}

	// User must authenticate with an access token
	if config.OAuthToken.AccessToken == "" {
		config.OAuthToken = *TokenFromWeb(*config)
		WriteConfig(*config_file, *config)
	}
	oauth_client := getOAuthClient(*config)

	// User must select id of folder to upload files into.
	if config.RemoteParentFolderId == "" {
		fmt.Println(
			"RemoteParentFolderId must be configured in config_file.\n",
			"You can use 'root' to use no folders. Otherwise, select a folder id ",
			"from the list below:")
		ListGDriveFolders(oauth_client)
		return false
	}

	if config.LocalScanDir == "" {
		fmt.Println(
			"LocalScanDir must be configured in config_file.\n",
			"This is the path which ScanServer scans to find new files to upload.")
		return false
	}

	// If LastProcessedScanTime is unset, we just start by uploading all. No need
	// for user input in this case.
	return true
}

// Taken from Google API Example Code
func openUrl(url string) {
	try := []string{"xdg-open", "google-chrome", "open"}
	for _, bin := range try {
		err := exec.Command(bin, url).Run()
		if err == nil {
			return
		}
	}
	log.Printf("Error opening URL in browser.")
}

// Slightly modified from Google API Example Code
func TokenFromWeb(config ScanServerConfig) *oauth.Token {
	ch := make(chan string)
	randState := fmt.Sprintf("st%d", time.Now().UnixNano())
	ts := httptest.NewServer(
		http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
			if req.URL.Path == "/favicon.ico" {
				http.Error(rw, "", 404)
				return
			}
			if req.FormValue("state") != randState {
				log.Printf("State doesn't match: req = %#v", req)
				http.Error(rw, "", 500)
				return
			}
			if code := req.FormValue("code"); code != "" {
				fmt.Fprintf(rw, "<h1>Success</h1>Authorized.")
				rw.(http.Flusher).Flush()
				ch <- code
				return
			}
			log.Printf("no code")
			http.Error(rw, "", 500)
		}))
	defer ts.Close()

	var oauth_config = &oauth.Config{
		ClientId:     config.ClientId,
		ClientSecret: config.ClientSecret,
		Scope:        drive.DriveScope,
		AuthURL:      "https://accounts.google.com/o/oauth2/auth",
		TokenURL:     "https://accounts.google.com/o/oauth2/token",
	}

	oauth_config.RedirectURL = ts.URL
	authUrl := oauth_config.AuthCodeURL(randState)
	go openUrl(authUrl)
	log.Printf("Authorize this app at: %s", authUrl)
	code := <-ch
	log.Printf("Got code: %s", code)

	t := &oauth.Transport{
		Config:    oauth_config,
		Transport: http.DefaultTransport,
	}
	_, err := t.Exchange(code)
	if err != nil {
		panic(fmt.Sprintf("Token exchange error: %v", err))
	}
	return t.Token
}

// Lists all of the client's GDrive Folders by Id.
func ListGDriveFolders(client *http.Client) {
	service, _ := drive.New(client)

	folder_query := "mimeType = 'application/vnd.google-apps.folder'"
	files_list, err := service.Files.List().MaxResults(1000).Q(folder_query).Do()
	if err != nil {
		panic(fmt.Sprintf("error listing folders: %v", err))
	}
	for _, file := range files_list.Items {
		fmt.Println("Id:", file.Id, " Title:", file.Title)
	}
}

func PeriodicallyListScans(config ScanServerConfig, files_chan chan string) {
	max_processed_time := config.LastProccessedScanTime
	for {
		files, err := ioutil.ReadDir(config.LocalScanDir)
		if err != nil {
			panic(err)
		}

		for _, file := range files {
			// We don't recurse into subdirs currently.
			if file.IsDir() {
				continue
			}
			// This skips files we've already processed
			if !max_processed_time.Before(file.ModTime()) {
				continue
			}
			// Track the maximum modification time we've seen so far.
			if file.ModTime().After(max_processed_time) {
				max_processed_time = file.ModTime()
			}

			files_chan <- filepath.Join(config.LocalScanDir, file.Name())
		}
		time.Sleep(5 * time.Second)
	}
}

func UploadFiles(config ScanServerConfig,
	files_to_upload_chan chan string,
	files_done_chan chan string) {
	client := getOAuthClient(config)
	service, _ := drive.New(client)

	for file_to_upload := range files_to_upload_chan {
		var go_file *os.File
		var err error
		var modified_time time.Time
		// HACK: If the file is still being created, it may be incomplete. Uploading
		// it may end up with a partial copy or a panic by the google api. We look
		// for a stable file size to indicate that the file has been completely
		// written before continuing.
		for {
			go_file, err = os.Open(file_to_upload)
			if err != nil {
				panic(fmt.Sprintf("error opening file: %v", err))
			}
			file_stat, err := go_file.Stat()
			if err != nil {
				panic(fmt.Sprintf("error examining file stat: %v", err))
			}
			if modified_time == file_stat.ModTime() {
				break
			} else {
				modified_time = file_stat.ModTime()
				time.Sleep(2 * time.Second)
			}
		}

		file_meta := &drive.File{
			Title:    filepath.Base(file_to_upload),
			MimeType: "application/pdf"}

		// Set the parent folder so that these files don't just get uploaded into
		// the root directory.
		parent := &drive.ParentReference{Id: config.RemoteParentFolderId}
		file_meta.Parents = []*drive.ParentReference{parent}

		_, err = service.Files.Insert(file_meta).Media(go_file).Do()
		if err != nil {
			panic(fmt.Sprintf("error uploading file: %v", err))
		}

		files_done_chan <- file_to_upload

		if config.LastProccessedScanTime.Before(modified_time) {
			config.LastProccessedScanTime = modified_time
			WriteConfig(*config_file, config)
		}

	}
	close(files_done_chan)
}

func getOAuthClient(config ScanServerConfig) *http.Client {
	var oauth_config = &oauth.Config{
		ClientId:     config.ClientId,
		ClientSecret: config.ClientSecret,
		Scope:        drive.DriveScope,
		AuthURL:      "https://accounts.google.com/o/oauth2/auth",
		TokenURL:     "https://accounts.google.com/o/oauth2/token",
	}

	transport := &oauth.Transport{
		Token:     &config.OAuthToken,
		Config:    oauth_config,
		Transport: http.DefaultTransport,
	}
	return transport.Client()
}
