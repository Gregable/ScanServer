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
	"path"
	"strings"
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

type FileForUpload struct {
	Path                string
	FileName            string
	FinalPathToCleanup  string
	PreferredUploadName string
}

func FullPath(file_for_upload FileForUpload) string {
	return path.Join(file_for_upload.Path,
		file_for_upload.FileName)
}

func IsDuplexFile(file_for_upload FileForUpload,
	config ScanServerConfig) bool {
	if config.DuplexPrefix == "" {
		return false
	}
	return strings.HasPrefix(file_for_upload.FileName, config.DuplexPrefix)
}

func UploadTitle(file_for_upload FileForUpload) string {
	if file_for_upload.PreferredUploadName != "" {
		return file_for_upload.PreferredUploadName
	}
	return file_for_upload.FileName
}

func main() {
	flag.Parse()

	config := ParseConfig(*config_file)
	if !PromptIfMissingConfigFields(&config) {
		return
	}

	// Output files that we find to found_files_chan
	found_files_chan := make(chan FileForUpload)
	go PeriodicallyListScans(config, found_files_chan)

	done_upload_chan := make(chan FileForUpload)
	if config.DuplexPrefix != "" {
		to_upload_chan := make(chan FileForUpload)
		go MergeDuplexScans(config, found_files_chan, to_upload_chan)
		go UploadFiles(config, to_upload_chan, done_upload_chan)
	} else {
		go UploadFiles(config, found_files_chan, done_upload_chan)
	}
	all_done_chan := make(chan FileForUpload)
	go CleanupTmpDirs(done_upload_chan, all_done_chan)

	for file := range all_done_chan {
		fmt.Println(
			"Successfully Uploaded file", file.FileName, "as", UploadTitle(file))
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

	if config.DuplexPrefix != "" && config.TmpDir == "" {
		fmt.Println(
			"If DuplexPrefix is configured in config_file, so must TmpDir.\n",
			"This is the path which ScanServer uses to build a temporary merged pdf.")
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

func PeriodicallyListScans(config ScanServerConfig,
	files_chan chan FileForUpload) {
	max_processed_time := config.LastProccessedScanTime
	for {
		files, err := ioutil.ReadDir(config.LocalScanDir)
		if err != nil {
			panic(err)
		}

		// We may pick up multiple files between scans, so we track the max time of
		// the last pass as well as this pass. Files since the last pass time will
		// be added to files_chan.
		this_pass_max_processed_time := max_processed_time
		for _, file := range files {
			// We don't recurse into subdirs currently.
			if file.IsDir() {
				continue
			}
			// This skips files we've already processed
			if !max_processed_time.Before(file.ModTime()) {
				continue
			}

			var file_for_upload FileForUpload
			file_for_upload.FileName = file.Name()
			file_for_upload.Path = config.LocalScanDir
			BlockUntilModificationTimeStable(FullPath(file_for_upload))

			// Track the maximum modification time we've seen so far.
			modified_time := ModifyTimeOrPanic(FullPath(file_for_upload))
			if modified_time.After(this_pass_max_processed_time) {
				this_pass_max_processed_time = modified_time
			}

			files_chan <- file_for_upload
		}
		max_processed_time = this_pass_max_processed_time
		time.Sleep(5 * time.Second)
	}
}

// Helper for MergeDuplexScans which clears temporary directories created once
// they are no longer needed.
func CleanupTmpDirs(files_to_cleanup_chan chan FileForUpload,
	done_chan chan FileForUpload) {
	for file_to_cleanup := range files_to_cleanup_chan {
		if file_to_cleanup.FinalPathToCleanup != "" {
			os.RemoveAll(file_to_cleanup.FinalPathToCleanup)
		}
		done_chan <- file_to_cleanup
	}
}

// Reads from an incoming channel for pairs of front/back files, merges them
// using command line methods, and then adds the merged file to the final
// upload channel.
func MergeDuplexScans(config ScanServerConfig,
	files_to_merge_chan chan FileForUpload,
	files_to_upload_chan chan FileForUpload) {

FrontLoop:
	for front_side_file := range files_to_merge_chan {
		// If it's not duplex, simply schedule for upload. Simple case.
		if !IsDuplexFile(front_side_file, config) {
			files_to_upload_chan <- front_side_file
			continue FrontLoop
		}

	BackLoop:
		for {
			select {
			case back_side_file := <-files_to_merge_chan:
				// We assume we won't get the first side of duplex Doc A followed by a
				// non-duplex Doc B. If we see something like this, we simply treat both
				// files as monoplex and upload both.
				if !IsDuplexFile(back_side_file, config) {
					files_to_upload_chan <- front_side_file
					files_to_upload_chan <- back_side_file
					continue FrontLoop
				}

				log.Println("Merging:", front_side_file.FileName, "and",
					back_side_file.FileName)

				tmp_dir, err := ioutil.TempDir(config.TmpDir, "")
				if err != nil {
					panic(err)
				}

				var merged_file FileForUpload
				merged_file.Path = tmp_dir
				merged_file.FinalPathToCleanup = tmp_dir
				merged_file.PreferredUploadName = "merged_" + front_side_file.FileName
				merged_file.FileName, err = MergeScans(
					FullPath(front_side_file), FullPath(back_side_file), tmp_dir)
				// If we get an error, it's probably because the files have different
				// numbers of pages. In this case, it's safer to upload them as two
				// unmerged monoplex files.
				if err != nil {
					os.RemoveAll(tmp_dir)
					files_to_upload_chan <- front_side_file
					front_side_file = back_side_file
					continue BackLoop // wait for next back_side_file
				}

				files_to_upload_chan <- merged_file
				continue FrontLoop

			// If an 15 min have elapsed, we aren't going to see the paired back side
			// file, so go ahead and release the front side file as a monoplex file.
			// This will help us to avoid pairing the wrong front/back files
			case <-time.After(15 * time.Minute):
				files_to_upload_chan <- front_side_file
				continue FrontLoop
			}
		}
	}
	close(files_to_upload_chan)
}

func ModifyTimeOrPanic(file_path string) time.Time {
	go_file, err := os.Open(file_path)
	if err != nil {
		panic(fmt.Sprintf("error opening file: %v", err))
	}

	file_stat, err := go_file.Stat()
	if err != nil {
		panic(fmt.Sprintf("error examining file stat: %v", err))
	}
	return file_stat.ModTime()
}

// HACK: If the file is still being created, it may be incomplete. Processing
// it may end up with a partial copy or a panic by the google api. We look
// for a stable modify time to indicate that the file has been completely
// written before continuing.
func BlockUntilModificationTimeStable(file_path string) {
	var modified_time time.Time
	for {
		if modified_time == ModifyTimeOrPanic(file_path) {
			break
		} else {
			modified_time = ModifyTimeOrPanic(file_path)
			// We want to sleep long enough to make sure that we see any change in the
			// document between accesses, but as short as possible so that we don't
			// introduce unnessary latency into the scanning process. 10s is arbitrary
			// here.
			//
			// I was worried that it was possible that the user is sitting at the
			// flatbed scanner inputting pages one at at time with significant delays
			// in between and that the scanner would send the pages incrementally. At
			// least in small tests (3 pages) this doesn't seem to happen. The scanner
			// waits until it has all of the pages (user confirmed) and then uploads. 
			// It's possible that it still happens if the scanner runs out of memory.
			//  
			// TODO: If we see a new document with a later modification time, it's
			// probably same to assume that this one is now done and we can progress
			// to waiting on the new document. Or at the very least process files in
			// parallel or something as this can cause minute delays to pile up in
			// the face of lots of scans.
			time.Sleep(10 * time.Second)
		}
	}
}

func UploadFiles(config ScanServerConfig,
	files_to_upload_chan chan FileForUpload,
	files_done_chan chan FileForUpload) {
	client := getOAuthClient(config)
	service, _ := drive.New(client)

	for file_for_upload := range files_to_upload_chan {
		go_file, err := os.Open(FullPath(file_for_upload))
		if err != nil {
			panic(fmt.Sprintf("error opening file: %v", err))
		}

		file_meta := &drive.File{
			Title:    UploadTitle(file_for_upload),
			MimeType: "application/pdf"}

		// Set the parent folder so that these files don't just get uploaded into
		// the root directory.
		parent := &drive.ParentReference{Id: config.RemoteParentFolderId}
		file_meta.Parents = []*drive.ParentReference{parent}

		_, err = service.Files.Insert(file_meta).Media(go_file).Do()
		if err != nil {
			panic(fmt.Sprintf("error uploading file: %v", err))
		}

		modified_time := ModifyTimeOrPanic(FullPath(file_for_upload))
		if config.LastProccessedScanTime.Before(modified_time) {
			config.LastProccessedScanTime = modified_time
			WriteConfig(*config_file, config)
		}

		files_done_chan <- file_for_upload
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
