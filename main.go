package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/mattetti/m3u8Grabber/m3u8"
)

var (
	debugFlag = flag.Bool("debug", false, "Set debug mode")
	dlAllFlag = flag.Bool("all", false, "Download all episodes if the page contains multiple videos.")
)

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

	givenURL := os.Args[1]
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
	// start the workers
	w := &sync.WaitGroup{}
	stopChan := make(chan bool)
	m3u8.LaunchWorkers(w, stopChan)

	// let's get all the videos for the replay page
	if strings.Contains(givenURL, "replay-videos") {
		urls := collectionURLs(givenURL, nil)
		for _, pageURL := range urls {
			downloadVideo(pageURL)
		}
	} else {
		downloadVideo(givenURL)
	}

	close(m3u8.DlChan)
	w.Wait()
}

func downloadVideo(givenURL string) {
	res, err := http.Get(givenURL)
	if err != nil {
		log.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		log.Printf("Can't download %s\nStatus code error: %d %s", givenURL, res.StatusCode, res.Status)
		return
	}

	doc, err := goquery.NewDocumentFromReader(res.Body)
	if err != nil {
		log.Fatal(err)
	}

	scriptText := doc.Find("body > div.l-content > div.l-two-columns > div.l-column-left > script").Text()
	scriptText = strings.TrimSpace(scriptText)
	if !strings.HasPrefix(scriptText, "var FTVPlayerVideos") {
		log.Fatalf("Unexpected script content, expected to find var FTVPlayerVideos\nMake sure you picked an episode page.\nfound script:%s\n", scriptText)
	}
	startIDX := strings.Index(scriptText, "[")
	endIDX := strings.LastIndex(scriptText, ";")
	jsonString := scriptText[startIDX:endIDX]
	var data []VideoData
	if err := json.Unmarshal([]byte(jsonString), &data); err != nil {
		log.Fatalf("Failed to parse json data:\n%s\nerr: %v", jsonString, err)
	}
	if *debugFlag {
		fmt.Println("Video Title:", data[0].VideoTitle)
		fmt.Printf("Video Id: %#v\n", data[0].VideoID)
	}

	apiURL := fmt.Sprintf("https://player.webservices.francetelevisions.fr/v1/videos/%s?country_code=FR&w=720&h=405&version=5.29.3&domain=www.france.tv&evice_type=desktop&browser=safari&browser_version=78&os=macos&os_version=10_14_6&diffusion_mode=tunnel_first", data[0].VideoID)

	res2, err := http.Get(apiURL)
	if err != nil {
		log.Fatal(err)
	}
	if res2.StatusCode != 200 {
		log.Printf("Stream for %s not available: %d %s", givenURL, res2.StatusCode, res2.Status)
		return
	}
	var stream StreamData
	err = json.NewDecoder(res2.Body).Decode(&stream)
	if err != nil {
		log.Fatalf("Failed to parse response data\nerr: %v", err)
	}
	res2.Body.Close()
	filename := fmt.Sprintf("%s - %s - %s", stream.Meta.Title, strings.ReplaceAll(stream.Meta.PreTitle, " ", ""), stream.Meta.AdditionalTitle)

	pathToUse, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}

	var manifestURL string
	if stream.Video.Token == "" {
		manifestURL = stream.Video.URL
	} else {
		tokenURL := strings.Replace(stream.Video.Token, "format=json", "format=text", 1)
		res3, err := http.Get(tokenURL)
		if err != nil {
			log.Fatal(err)
		}
		defer res3.Body.Close()
		if res3.StatusCode != 200 {
			log.Printf("Stream for %s not available: %d %s", givenURL, res3.StatusCode, res3.Status)
			return
		}

		b, err := ioutil.ReadAll(res3.Body)
		if err != nil {
			panic(err)
		}
		manifestURL = string(b)
	}

	if stream.Video.Format == "hls" {
		job := &m3u8.WJob{
			Type: m3u8.ListDL,
			URL:  manifestURL,
			// SkipConverter: true,
			DestPath: pathToUse,
			Filename: filename}
		m3u8.DlChan <- job
		return
	}

	// if stream.Video.Format == "dash" {
	// https://godoc.org/github.com/zencoder/go-dash/mpd
	// 	stream.Video.Token
	// }
	fmt.Printf("%s is in an unsupported format: %s\n", filename, stream.Video.Format)
	fmt.Printf("Data: %s\n", apiURL)
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

	doc.Find("#videos > div > div > h3 > a").Each(func(i int, s *goquery.Selection) {
		count++
		href, _ := s.Attr("href")
		videoPageURL := fmt.Sprintf("https://france.tv%s", href)
		fmt.Println("Do you want to download", s.Text(), "? (Type y for Yes)")
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

	if count > 0 {
		if !strings.Contains(givenURL, "ajax/?page") {
			fmt.Println("Checking pagination")
			return collectionURLs(givenURL+"ajax/?page=1", episodeURLs)
		}
	}

	return episodeURLs
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
	SeasonNumber string      `json:"seasonNumber"`
}

type StreamData struct {
	Video struct {
		Workflow           string        `json:"workflow"`
		Token              string        `json:"token"`
		Duration           int           `json:"duration"`
		Embed              bool          `json:"embed"`
		Format             string        `json:"format"`
		OfflineRights      bool          `json:"offline_rights"`
		IsLive             bool          `json:"is_live"`
		Drm                interface{}   `json:"drm"`
		PlayerVerification bool          `json:"player_verification"`
		IsDVR              bool          `json:"is_DVR"`
		Spritesheets       []interface{} `json:"spritesheets"`
		IsStartoverEnabled bool          `json:"is_startover_enabled"`
		ComingNext         struct {
			Timecode int `json:"timecode"`
			Duration int `json:"duration"`
		} `json:"coming_next"`
		URL      string        `json:"url"`
		Captions []interface{} `json:"captions"`
	} `json:"video"`
	Meta struct {
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