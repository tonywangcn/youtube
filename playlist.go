package youtube

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	sjson "github.com/bitly/go-simplejson"
	"golang.org/x/net/html"
)

const (
	playlistFetchURL string = "https://www.youtube.com/playlist?list=%s&hl=en"
	// The following are used in tests but also for fetching test data
	testPlaylistResponseDataFile = "./testdata/playlist_test_data.html"
	testPlaylistID               = "PL59FEE129ADFF2B12"
)

var (
	playlistIDRegex    = regexp.MustCompile("^[A-Za-z0-9_-]{18,42}$")
	playlistInURLRegex = regexp.MustCompile("[&?]list=([A-Za-z0-9_-]{18,42})(&.*)?$")
)

type videoType struct {
	URI     string
	Pattern string
}

var videoTypeMap = []videoType{
	videoType{"https://www.youtube.com/playlist?list=%v", `(list|p)=([^/&]+)`},
	videoType{"https://www.youtube.com/c/%v/videos", `/(c)/([^/&]+)/videos`},
	videoType{"https://www.youtube.com/channel/%v/videos", `/(channel)/([^/&]+)/videos`},
	videoType{"https://www.youtube.com/user/%v/videos", `/(user)/([^/&]+)/videos`},
	videoType{"https://www.youtube.com/%v/videos", `/(www.youtube.com)/([^/&]+)/videos`},
}

// MatchOneOf match one of the patterns
func MatchOneOf(text string, patterns ...string) []string {
	var (
		re    *regexp.Regexp
		value []string
	)
	for _, pattern := range patterns {
		// (?flags): set flags within current group; non-capturing
		// s: let . match \n (default false)
		// https://github.com/google/re2/wiki/Syntax
		re = regexp.MustCompile(pattern)
		value = re.FindStringSubmatch(text)
		if len(value) > 0 {
			return value
		}
	}
	return nil
}

func getVideoType(uri string) (string, error) {
	for video := range videoTypeMap {
		re := MatchOneOf(uri, videoTypeMap[video].Pattern)
		if re != nil && len(re) >= 3 && len(re[2]) > 0 {
			return fmt.Sprintf(videoTypeMap[video].URI, re[2]), nil
		}
	}
	return "", errors.New("failed to parse id from URL")
}

type Playlist struct {
	ID          string
	Title       string
	Author      string
	Description string
	Link        string
	Image       string
	PubDate     time.Time
	Videos      []*PlaylistEntry
}

type PlaylistEntry struct {
	ID       string
	Title    string
	Author   string
	Duration time.Duration
}

func extractPlaylistID(url string) (string, error) {
	if playlistIDRegex.Match([]byte(url)) {
		return url, nil
	}

	matches := playlistInURLRegex.FindStringSubmatch(url)

	if matches != nil {
		return matches[1], nil
	}

	return "", ErrInvalidPlaylist
}

func extractPlaylistJSON(r io.Reader) ([]byte, error) {
	const prefix = "var ytInitialData ="

	doc, err := html.Parse(r)
	if err != nil {
		return nil, err
	}
	var data []byte
	var f func(*html.Node)
	f = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "script" && n.FirstChild != nil {
			script := n.FirstChild.Data
			if strings.HasPrefix(script, prefix) {
				script = strings.TrimPrefix(script, prefix)
				data = []byte(strings.Trim(script, ";"))
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			f(c)
		}
	}
	f(doc)
	return data, nil
}

// structs for playlist extraction

// Title: metadata.playlistMetadataRenderer.title | sidebar.playlistSidebarRenderer.items[0].playlistSidebarPrimaryInfoRenderer.title.runs[0].text
// Author: sidebar.playlistSidebarRenderer.items[1].playlistSidebarSecondaryInfoRenderer.videoOwner.videoOwnerRenderer.title.runs[0].text

// Videos: contents.twoColumnBrowseResultsRenderer.tabs[0].tabRenderer.content.sectionListRenderer.contents[0].itemSectionRenderer.contents[0].playlistVideoListRenderer.contents
// ID: .videoId
// Title: title.runs[0].text
// Author: .shortBylineText.runs[0].text
// Duration: .lengthSeconds

// TODO?: Author thumbnails: sidebar.playlistSidebarRenderer.items[0].playlistSidebarPrimaryInfoRenderer.thumbnailRenderer.playlistVideoThumbnailRenderer.thumbnail.thumbnails
// TODO? Video thumbnails: .thumbnail.thumbnails

func (p *Playlist) UnmarshalJSON(b []byte) (err error) {
	var j *sjson.Json
	j, err = sjson.NewJson(b)
	if err != nil {
		return err
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("JSON parsing error: %v", r)
		}
	}()
	p.Title = j.GetPath("metadata", "playlistMetadataRenderer", "title").MustString()
	if p.Title == "" {
		p.Title = j.GetPath("metadata", "channelMetadataRenderer", "title").MustString()
	}

	p.Author = j.GetPath("sidebar", "playlistSidebarRenderer", "items").GetIndex(1).
		GetPath("playlistSidebarSecondaryInfoRenderer", "videoOwner", "videoOwnerRenderer", "title", "runs").
		GetIndex(0).Get("text").MustString()
	if p.Author == "" {
		p.Author = p.Title
	}

	p.Description = j.GetPath("metadata", "channelMetadataRenderer", "description").MustString()
	p.Image = j.GetPath("metadata", "channelMetadataRenderer", "avatar", "thumbnails").GetIndex(0).GetPath("url").MustString()

	vJSON, err := j.GetPath("contents", "twoColumnBrowseResultsRenderer", "tabs").GetIndex(0).
		GetPath("tabRenderer", "content", "sectionListRenderer", "contents").GetIndex(0).
		GetPath("itemSectionRenderer", "contents").GetIndex(0).
		GetPath("playlistVideoListRenderer", "contents").MarshalJSON()

	fmt.Printf("playlist %+v", p)
	var vids []*videosJSONExtractor
	if err := json.Unmarshal(vJSON, &vids); err != nil {
		return err
	}
	if len(vids) == 0 {
		vJSON, err = j.GetPath("contents", "twoColumnBrowseResultsRenderer", "tabs").GetIndex(1).
			GetPath("tabRenderer", "content", "sectionListRenderer", "contents").GetIndex(0).
			GetPath("itemSectionRenderer", "contents").GetIndex(0).
			GetPath("gridRenderer", "items").MarshalJSON()
		if err := json.Unmarshal(vJSON, &vids); err != nil {
			fmt.Printf("err %v", err)
			return err
		}
	}
	fmt.Println("vids ", vids)
	p.Videos = make([]*PlaylistEntry, 0, len(vids))
	for _, v := range vids {

		if v.Renderer != nil || v.ChannelRenderer != nil {
			fmt.Println("PlaylistEntry ", v.PlaylistEntry())
			p.Videos = append(p.Videos, v.PlaylistEntry())
		}

	}
	return nil
}

type videosJSONExtractor struct {
	Renderer *struct {
		ID       string   `json:"videoId"`
		Title    withRuns `json:"title"`
		Author   withRuns `json:"shortBylineText"`
		Duration string   `json:"lengthSeconds"`
	} `json:"playlistVideoRenderer"`
	ChannelRenderer *struct {
		ID                string   `json:"videoId"`
		Title             withRuns `json:"title"`
		Author            withRuns `json:"shortBylineText"`
		ThumbnailOverlays []struct {
			ThumbnailOverlayTimeStatusRenderer struct {
				Text struct {
					SimpleText string `json:"simpleText"`
				} `json:"text"`
			} `json:"thumbnailOverlayTimeStatusRenderer"`
		} `json:"thumbnailOverlays"`
	} `json:"gridVideoRenderer"`
}

func (vje videosJSONExtractor) PlaylistEntry() *PlaylistEntry {
	if vje.Renderer != nil {
		ds, err := strconv.Atoi(vje.Renderer.Duration)
		if err != nil {
			panic("invalid video duration: " + vje.Renderer.Duration)
		}
		return &PlaylistEntry{
			ID:       vje.Renderer.ID,
			Title:    vje.Renderer.Title.String(),
			Author:   vje.Renderer.Author.String(),
			Duration: time.Second * time.Duration(ds),
		}
	} else {
		timeStr := vje.ChannelRenderer.ThumbnailOverlays[0].ThumbnailOverlayTimeStatusRenderer.Text.SimpleText
		if strings.Count(timeStr, ":") == 1 {
			timeStr = "0:" + timeStr
		}
		ds, err := time.Parse("3:4:5", timeStr)
		if err != nil {
			fmt.Print("invalid video duration: " + timeStr)
		}
		fmt.Println("ds ", ds, "time ", time.Time{})
		return &PlaylistEntry{
			ID:       vje.ChannelRenderer.ID,
			Title:    vje.ChannelRenderer.Title.String(),
			Author:   vje.ChannelRenderer.Author.String(),
			Duration: ds.AddDate(1, 0, 0).Sub(time.Time{}),
		}
	}

}

type withRuns struct {
	Runs []struct {
		Text string `json:"text"`
	} `json:"runs"`
}

func (wr withRuns) String() string {
	if len(wr.Runs) > 0 {
		return wr.Runs[0].Text
	}
	return ""
}
