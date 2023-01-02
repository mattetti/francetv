package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/mattetti/m3u8Grabber/m3u8"
	"github.com/zencoder/go-dash/mpd"
)

var (
	debugFlag = flag.Bool("debug", false, "Set debug mode")
	dlAllFlag = flag.Bool("all", false, "Download all episodes if the page contains multiple videos.")
	subsOnly  = flag.Bool("subsOnly", false, "Only download the subtitles.")
	URLFlag   = flag.String("url", "", "URL of the page to backup.")
	hlsFlag   = flag.Bool("m3u8", true, "Should use HLS/m3u8 format to download (instead of dash)")
)

var ErrNoPlayerData = errors.New("no playerData found")
var ErrMissingPayerJSONData = errors.New("no JSON data found")
var ErrBadPlayerJSONData = errors.New("Bad JSON data found")

func main() {
	flag.Parse()
	if len(os.Args) < 2 {
		fmt.Println("you need to pass the URL of a FranceTV episode page.")
		fmt.Println("Take a look at https://www.france.tv/enfants/six-huit-ans/ for ideas")
		os.Exit(1)
	}
	if *debugFlag {
		m3u8.Debug = true
	}

	if *subsOnly {
		fmt.Println("Downloading subtitles only")
	}

	givenURL := *URLFlag
	u, err := url.Parse(givenURL)
	if err != nil {
		fmt.Println("Something went wrong when trying to parse", givenURL)
		fmt.Println(err)
		os.Exit(1)
	}
	if *debugFlag {
		fmt.Println("Checking", u)
	}
	if len(os.Args) > 2 {
		if os.Args[2] == "-all" {
			*dlAllFlag = true
		}
	}
	w := &sync.WaitGroup{}
	if *hlsFlag {
		// start the m3u8 workers
		stopChan := make(chan bool)
		m3u8.LaunchWorkers(w, stopChan)
	}

	// let's get all the videos for the replay page
	if strings.Contains(givenURL, "replay-videos") || strings.Contains(givenURL, "toutes-les-videos") {
		log.Println("Trying to find all videos")
		urls := collectionURLs(givenURL, nil)
		log.Printf("%d videos found in %s\n", len(urls), givenURL)
		for _, pageURL := range urls {
			if *hlsFlag {
				downloadHLSVideo(pageURL)
			} else {
				downloadDashVideo(pageURL)
			}
		}
	} else {
		if *hlsFlag {
			downloadHLSVideo(givenURL)
		} else {
			downloadDashVideo(givenURL)
		}
	}

	if *hlsFlag {
		close(m3u8.DlChan)
		w.Wait()
	}
}

func downloadDashVideo(givenURL string) {

	// 0. Parse the page to find the product/video IDs
	data, err := extractVideoDataFromPage(givenURL)
	if err != nil {
		// check if we have a collection page instead of a single item page
		if urls := collectionURLs(givenURL, nil); len(urls) > 0 {
			for _, pageURL := range urls {
				downloadDashVideo(pageURL)
			}
		}

		fmt.Println(err)
		os.Exit(1)
	}

	if data == nil {
		fmt.Println("No video data found in the page and an unexpected lack of error being reported")
		os.Exit(1)
	}
	// fmt.Printf("data: %#v\n", data)
	productID := data.ContentID
	videoID := data.VideoID
	originURL := data.OriginURL.(string)

	// 1. Call the API to get the manifest using the video and product IDs we just recovered
	stream, err := fetchMPDStreamInfo(videoID, productID, originURL)
	if err != nil {
		fmt.Println("Failed to retrieve the stream info using the FTV API")
		fmt.Println(err)
		os.Exit(1)
	}

	// 2. Using the stream data, prepare the request to get the mpd temp, signed URL

	// fmt.Printf("stream data: %+v\n", stream)

	preTitle := stream.Meta.PreTitle
	if preTitle == "" {
		preTitle = data.VideoTitle
	}
	preTitle = strings.ReplaceAll(preTitle, " ", "")
	filename := fmt.Sprintf("%s - %s - %s", stream.Meta.Title, preTitle, stream.Meta.AdditionalTitle)

	pathToUse, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}

	// 3. Download the mpd file
	manifestPath := filepath.Join(pathToUse, filename+".mpd")
	mpdf, err := downloadMPDFile(stream, manifestPath)
	if err != nil {
		fmt.Println("Failed to download the mpd file")
		fmt.Println(err)
		os.Exit(1)
	}
	defer mpdf.Close()

	mpdData, err := mpd.Read(mpdf)
	if err != nil {
		fmt.Printf("Failed to read the mpd file - %s\n", err)
		os.Exit(1)
	}

	if mpdData.Type != nil && (*mpdData.Type == "dynamic") {
		log.Println("dynamic mpd not supported")
		return
	}

	for _, period := range mpdData.Periods {
		fmt.Printf("%s, duration: %s\n\n", period.BaseURL, time.Duration(period.Duration).String())
		for _, adaptationSet := range period.AdaptationSets {
			fmt.Printf("Adaptation set ID: %s/%s - %s, mimeType: %s, lang: %s, codecs: %s \n",
				strPtr(adaptationSet.ID),
				strPtr(adaptationSet.Group),
				strPtr(adaptationSet.ContentType),
				strPtr(adaptationSet.MimeType),
				strPtr(adaptationSet.Lang),
				strPtr(adaptationSet.Codecs),
			)
			// var codecs string
			for _, r := range adaptationSet.Representations {
				switch *adaptationSet.ContentType {
				case "video":
					fmt.Printf("Rep ID: %s, Bandwidth: %d, width: %d, height: %d, codecs: %s, scanType: %s\n", strPtr(r.ID), int64Ptr(r.Bandwidth), int64Ptr(r.Width), int64Ptr(r.Height), strPtr(r.Codecs), strPtr(r.ScanType))
				case "audio":
					fmt.Printf("Rep ID: %s, Bandwidth: %d\n", strPtr(r.ID), int64Ptr(r.Bandwidth))
				case "text":
					fmt.Printf("Rep ID: %s\n", strPtr(r.ID))
				default:
					log.Printf("Unknown content type: %s", *adaptationSet.ContentType)
				}
			}
			fmt.Println()
		}
	}

	destPath := filepath.Join(pathToUse, filename+".mp4")
	if fileAlreadyExists(destPath) {
		fmt.Printf("%s already exists\n", destPath)
		return
	}
	fmt.Printf("Preparing to download to %s\n", destPath)

}

func strPtr(s *string) string {
	if s == nil {
		return "unknown"
	}
	return *s
}

func int64Ptr(d *int64) int {
	if d == nil {
		return 0
	}
	return int(*d)
}

// extractVideoDataFromPage extracts the video data used to then call the API
// the data is stored in the HTML page as a JSON object. This function extracts
// the JSON object and returns a VideoData struct.
// Note that the script location changes often and the lookup is quite fragile.
func extractVideoDataFromPage(givenURL string) (*VideoData, error) {
	res, err := http.Get(givenURL)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		log.Printf("Can't download %s\nStatus code error: %d %s", givenURL, res.StatusCode, res.Status)
		return nil, fmt.Errorf("Bad Status code fetching %s: %d %s", givenURL, res.StatusCode, res.Status)
	}

	doc, err := goquery.NewDocumentFromReader(res.Body)
	if err != nil {
		return nil, fmt.Errorf("Failed to parse the page: %s", err)
	}

	scriptText := doc.Find("div > div.l-column-left > script").Text()
	scriptText = strings.TrimSpace(scriptText)
	if !strings.HasPrefix(scriptText, "window.FTVPlayerVideos") && !strings.HasPrefix(scriptText, "let FTVPlayerVideos") {
		return nil, ErrNoPlayerData
	}
	startIDX := strings.Index(scriptText, "[")
	endIDX := strings.LastIndex(scriptText, ";")
	if startIDX < 0 || endIDX <= startIDX {
		log.Printf("Didn't find the expected json data in %s - %v\n", givenURL, scriptText)
		if *dlAllFlag {
			return nil, ErrMissingPayerJSONData
		}
	}
	log.Printf("Parsing the json data")
	jsonString := scriptText[startIDX:endIDX]
	var data []VideoData
	if err := json.Unmarshal([]byte(jsonString), &data); err != nil {
		log.Printf("Failed to parse json data:\n%s\nerr: %v", jsonString, err)
		return nil, ErrBadPlayerJSONData
	}

	return &data[0], nil
}

func fetchMPDStreamInfo(videoID string, productID int, originURL string) (*StreamData, error) {

	reqURL := fmt.Sprintf("https://k7.ftven.fr/videos/%s?country_code=FR&w=955&h=537&screen_w=1680&screen_h=1050&player_version=5.71.7&domain=www.france.tv&device_type=desktop&browser=chrome&browser_version=108&os=macos&os_version=10_15_7&diffusion_mode=tunnel_first&gmt=0100&video_product_id=%d", videoID, productID)

	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("could not create request for %s, err: %v", reqURL, err)
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "fr-FR;q=0.9,fr;q=0.8")
	req.Header.Set("Dnt", "1")
	req.Header.Set("Origin", "https://www.france.tv")
	req.Header.Set("Referer", fmt.Sprintf("https://www.france.tv%s", originURL))
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/108.0.0.0 Safari/537.36")
	req.Header.Set("Sec-Ch-Ua", "Chromium\";v=\"108\", \"Google Chrome\";v=\"108\"")
	req.Header.Set("Sec-Ch-Ua-Platform", "\"macOS\"")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request to %s, err: %v", reqURL, err)

	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("failed to download %s, status code: %d", reqURL, resp.StatusCode)
	}

	var stream StreamData
	err = json.NewDecoder(resp.Body).Decode(&stream)
	if err != nil {
		return nil, fmt.Errorf("failed to parse JSON response data\nerr: %v", err)
	}
	return &stream, nil
}

// download and returns the MPD file for th given stream. The caller is responsible for closing the file
func downloadMPDFile(stream *StreamData, outPath string) (*os.File, error) {

	var manifestURL string
	if stream.Video.Token == "" {
		if *debugFlag {
			fmt.Println("video token not set")
		}
		manifestURL = stream.Video.URL
	} else {
		tokenURL := fmt.Sprintf("%s&url=%s", stream.Video.Token, stream.Video.URL)
		tokenURL = strings.Replace(tokenURL, "format=json", "format=text", 1)
		resp, err := http.Get(tokenURL)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch the token URL %s - %s", tokenURL, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("stream for %s not available: %d %s", tokenURL, resp.StatusCode, resp.Status)
		}

		b, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read the token URL %s - %s", tokenURL, err)
		}
		manifestURL = string(b)
	}

	// we now have the mpd URL
	if *debugFlag {
		fmt.Println("MPD manifest URL", manifestURL)
	}
	f, err := downloadFile(manifestURL, outPath)
	if err != nil {
		return nil, fmt.Errorf("Failed to download the manifest url: %s - %s\n", manifestURL, err)
	}
	f.Seek(0, io.SeekStart)
	return f, nil
}

func getHLSManifestURL(stream *StreamData) (string, error) {
	var manifestURL string
	if stream.Video.Token == "" {
		manifestURL = stream.Video.URL
	} else {
		tokenURL := strings.Replace(stream.Video.Token, "format=json", "format=text", 1)
		resp, err := http.Get(tokenURL)
		if err != nil {
			return "", fmt.Errorf("failed to fetch the token URL %s - %s", tokenURL, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return "", fmt.Errorf("token API replied with a bad status code: %d %s", resp.StatusCode, resp.Status)
		}

		b, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return "", fmt.Errorf("failed to read the token URL %s - %s", tokenURL, err)
		}
		manifestURL = string(b)
	}
	return manifestURL, nil
}

func fetchHSLStreamInfo(videoID string) (*StreamData, error) {
	apiURL := fmt.Sprintf("https://player.webservices.francetelevisions.fr/v1/videos/%s?country_code=FR&w=1024&h=768&version=5.29.4&domain=www.france.tv&device_type=desktop&browser=safari&browser_version=13&os=macos&os_version=10_14_6&diffusion_mode=tunnel_first&gmt=%2B1", videoID)

	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("could not create request for %s, err: %v", apiURL, err)
	}
	req.Header.Set("Origin", "https://www.france.tv")
	// req.Header.Set("user-agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_14_6) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/13.0.1 Safari/605.1.15")
	req.Header.Set("Host", "player.webservices.francetelevisions.fr")
	req.Header.Set("Referer", "https://www.france.tv/")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request to %s, err: %v", apiURL, err)

	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("failed to download %s, status code: %d", apiURL, resp.StatusCode)
	}

	var stream StreamData
	err = json.NewDecoder(resp.Body).Decode(&stream)
	if err != nil {
		return nil, fmt.Errorf("failed to parse JSON response data\nerr: %v", err)
	}
	return &stream, nil
}

func downloadHLSVideo(givenURL string) {
	// 0. Parse the page to find the product/video IDs
	data, err := extractVideoDataFromPage(givenURL)
	if err != nil {
		// check if we have a collection page instead of a single item page
		if urls := collectionURLs(givenURL, nil); len(urls) > 0 {
			for _, pageURL := range urls {
				downloadHLSVideo(pageURL)
			}
		} else {
			log.Println("Unexpected script content, expected to find FTVPlayerVideos or a video player\nMake sure you picked an episode page.")
		}

		fmt.Println(err)
		if *dlAllFlag {
			return
		}

		os.Exit(1)
	}

	if data == nil {
		fmt.Println("No video data found in the page and an unexpected lack of error being reported")
		os.Exit(1)
	}

	// 1. Fetch the stream info using the FTV API
	stream, err := fetchHSLStreamInfo(data.VideoID)
	if err != nil {
		log.Println("something wrong happened when fetching the stream info", err)
		os.Exit(1)
	}

	if stream.Video.Format == "dash" {
		downloadDashVideo(givenURL)
		return
	}

	preTitle := stream.Meta.PreTitle
	if preTitle == "" {
		preTitle = data.VideoTitle
	}
	preTitle = strings.ReplaceAll(preTitle, " ", "")
	filename := fmt.Sprintf("%s - %s - %s", stream.Meta.Title, preTitle, stream.Meta.AdditionalTitle)

	pathToUse, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}

	destPath := filepath.Join(pathToUse, filename+".mp4")
	if fileAlreadyExists(destPath) {
		fmt.Printf("%s already exists\n", destPath)
		return
	}

	// 2. Fetch the actual manifest URL (m3u8)
	manifestURL, err := getHLSManifestURL(stream)
	if err != nil {
		log.Println("something wrong happened when fetching the manifest URL", err)
		os.Exit(1)
	}

	if *debugFlag {
		log.Printf("Manifest file: %s\n", manifestURL)
	}

	fmt.Printf("Queing up %s\n", destPath)

	// 3. Queue the video to download
	if stream.Video.Format == "hls" {
		job := &m3u8.WJob{
			Type:     m3u8.ListDL,
			URL:      manifestURL,
			SubsOnly: *subsOnly,
			// SkipConverter: true,
			DestPath: pathToUse,
			Filename: filename}
		m3u8.DlChan <- job
		return
	}

	fmt.Printf("%s is in an unsupported format: %s\n", filename, stream.Video.Format)
}

func collectionURLs(givenURL string, episodeURLs []string) []string {
	res, err := http.Get(givenURL)
	if err != nil {
		log.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		log.Fatalf("status code error: %d %s", res.StatusCode, res.Status)
	}

	// Load the HTML document
	doc, err := goquery.NewDocumentFromReader(res.Body)
	if err != nil {
		log.Fatal(err)
	}
	if episodeURLs == nil {
		episodeURLs = []string{}
	}
	count := 0

	doc.Find("a.c-card-16x9").Each(func(i int, s *goquery.Selection) {
		count++
		href, _ := s.Attr("href")
		videoPageURL := fmt.Sprintf("https://france.tv%s", href)
		title := s.Find(".c-card-16x9__subtitle").First().Text()
		fmt.Println("Do you want to download", title, "? (Type y for Yes)")
		if *dlAllFlag {
			episodeURLs = append(episodeURLs, videoPageURL)
		} else {
			reader := bufio.NewReader(os.Stdin)
			inputText, _ := reader.ReadString('\n')
			inputText = strings.TrimSpace(inputText)
			if inputText == "y" || inputText == "Y" {
				episodeURLs = append(episodeURLs, videoPageURL)
			}
		}
	})

	if count == 0 && len(episodeURLs) == 0 {
		log.Println("No videos found on this page")
	}

	if count > 0 {
		if !strings.Contains(givenURL, "?page") {
			fmt.Println("Checking pagination")
			return collectionURLs(givenURL+"/?page=1", episodeURLs)
		} else {
			idx := strings.LastIndex(givenURL, "page=")
			if idx > 0 {
				currentPage, err := strconv.Atoi(givenURL[idx+5:])
				if err != nil {
					fmt.Printf("Couldn't get the next page - %v", err)
					return episodeURLs
				}
				nextURL := givenURL[:idx+5] + strconv.Itoa(currentPage+1)
				return collectionURLs(nextURL, episodeURLs)
			}
		}
	}

	return episodeURLs
}

func downloadFile(url string, path string) (*os.File, error) {
	// Create the file
	out, err := os.Create(path)
	if err != nil {
		return nil, err
	}

	// for mpd
	// "Accept", "application/dash+xml,video/vnd.mpeg.dash.mpd"

	// Get the data
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Check server response
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bad status: %s", resp.Status)
	}

	// Write the body to file
	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return nil, err
	}

	return out, nil
}

func fileAlreadyExists(path string) bool {
	_, err := os.Stat(path)
	return !os.IsNotExist(err)
}

type VideoData struct {
	ContentID int       `json:"contentId"`
	VideoID   string    `json:"videoId"`
	EndDate   time.Time `json:"endDate"`
	Tracking  struct {
		Offre          string `json:"offre"`
		Support        string `json:"support"`
		EventType      string `json:"event_type"`
		Level2         string `json:"level_2"`
		EventPage      string `json:"event_page"`
		EventChapitre1 string `json:"event_chapitre1"`
		EventChapitre2 string `json:"event_chapitre2"`
	} `json:"tracking"`
	OriginURL interface{} `json:"originUrl"`
	// ComingNext []struct {
	// 	Title    string `json:"title"`
	// 	PreTitle string `json:"preTitle"`
	// 	Program  string `json:"program"`
	// 	Image    string `json:"image"`
	// } `json:"comingNext"`
	IsSponsored  bool        `json:"isSponsored"`
	IsAdVisible  interface{} `json:"isAdVisible"`
	VideoTitle   string      `json:"videoTitle"`
	ProgramName  string      `json:"programName"`
	SeasonNumber int         `json:"seasonNumber"`
}

type StreamDataVideo struct {
	Workflow           string        `json:"workflow"`
	Token              string        `json:"token"`
	Duration           int           `json:"duration"`
	Embed              bool          `json:"embed"`
	Format             string        `json:"format"`
	OfflineRights      bool          `json:"offline_rights"`
	IsLive             bool          `json:"is_live"`
	Drm                interface{}   `json:"drm"`
	DrmType            interface{}   `json:"drm_type"`
	LicenseType        interface{}   `json:"license_type"`
	PlayerVerification bool          `json:"player_verification"`
	IsDVR              bool          `json:"is_DVR"`
	Spritesheets       []interface{} `json:"spritesheets"`
	IsStartoverEnabled bool          `json:"is_startover_enabled"`
	Previously         struct {
		Timecode          interface{} `json:"timecode"`
		Duration          interface{} `json:"duration"`
		TimeBeforeDismiss interface{} `json:"time_before_dismiss"`
	} `json:"previously"`
	ComingNext struct {
		Timecode int `json:"timecode"`
		Duration int `json:"duration"`
	} `json:"coming_next"`
	SkipIntro struct {
		Timecode          interface{} `json:"timecode"`
		Duration          interface{} `json:"duration"`
		TimeBeforeDismiss interface{} `json:"time_before_dismiss"`
	} `json:"skip_intro"`
	Timeshiftable interface{}   `json:"timeshiftable"`
	URL           string        `json:"url"`
	DaiType       interface{}   `json:"dai_type"`
	Captions      []interface{} `json:"captions"`
	Offline       interface{}   `json:"offline"`
}

type StreamData struct {
	Video StreamDataVideo `json:"video"`
	Meta  struct {
		ID              string    `json:"id"`
		PlurimediaID    string    `json:"plurimedia_id"`
		Title           string    `json:"title"`
		AdditionalTitle string    `json:"additional_title"`
		PreTitle        string    `json:"pre_title"`
		BroadcastedAt   time.Time `json:"broadcasted_at"`
		ImageURL        string    `json:"image_url"`
	} `json:"meta"`
	Streamroot struct {
		Enabled   bool   `json:"enabled"`
		ContentID string `json:"content_id"`
		Property  string `json:"property"`
		License   string `json:"license"`
	} `json:"streamroot"`
	Markers struct {
		Analytics struct {
			IsTrackingEnabled bool   `json:"isTrackingEnabled"`
			IDSite            int    `json:"idSite"`
			Variant           string `json:"variant"`
			GeneralPlacement  string `json:"generalPlacement"`
			DetailedPlacement string `json:"detailedPlacement"`
			URL               string `json:"url"`
			Server            string `json:"server"`
			DiffusionDate     string `json:"diffusionDate"`
			ProgramName       string `json:"programName"`
			VideoTitle        string `json:"videoTitle"`
			Season            int    `json:"season"`
		} `json:"analytics"`
		Estat struct {
			CrmID          string      `json:"crmID"`
			Dom            interface{} `json:"dom"`
			Level1         string      `json:"level1"`
			Level2         string      `json:"level2"`
			Level3         string      `json:"level3"`
			Level4         string      `json:"level4"`
			Level5         interface{} `json:"level5"`
			Serial         string      `json:"serial"`
			StreamGenre    string      `json:"streamGenre"`
			StreamName     string      `json:"streamName"`
			StreamDuration int         `json:"streamDuration"`
			NewLevel1      string      `json:"newLevel1"`
			NewLevel2      string      `json:"newLevel2"`
			NewLevel3      string      `json:"newLevel3"`
			NewLevel4      string      `json:"newLevel4"`
			NewLevel5      string      `json:"newLevel5"`
			NewLevel6      string      `json:"newLevel6"`
			NewLevel7      interface{} `json:"newLevel7"`
			NewLevel8      string      `json:"newLevel8"`
			NewLevel9      interface{} `json:"newLevel9"`
			NewLevel10     interface{} `json:"newLevel10"`
			NewLevel11     interface{} `json:"newLevel11"`
			NewLevel12     interface{} `json:"newLevel12"`
			NewLevel13     interface{} `json:"newLevel13"`
			NewLevel14     interface{} `json:"newLevel14"`
			NewLevel15     interface{} `json:"newLevel15"`
			MediaContentID string      `json:"mediaContentId"`
			MediaDiffMode  string      `json:"mediaDiffMode"`
			MediaChannel   string      `json:"mediaChannel"`
			NetMeasurement string      `json:"netMeasurement"`
		} `json:"estat"`
		Npaw struct {
			CustomDimension1 string      `json:"customDimension1"`
			CustomDimension2 string      `json:"customDimension2"`
			CustomDimension3 string      `json:"customDimension3"`
			CustomDimension4 string      `json:"customDimension4"`
			CustomDimension5 string      `json:"customDimension5"`
			CustomDimension6 string      `json:"customDimension6"`
			CustomDimension7 interface{} `json:"customDimension7"`
			CustomDimension8 string      `json:"customDimension8"`
		} `json:"npaw"`
		Pub struct {
			Csid            string        `json:"csid"`
			Caid            string        `json:"caid"`
			Afid            string        `json:"afid"`
			Sfid            string        `json:"sfid"`
			MidrollTimecode []interface{} `json:"midroll_timecode"`
			IsPreview6H     bool          `json:"isPreview6h"`
		} `json:"pub"`
	} `json:"markers"`
}
