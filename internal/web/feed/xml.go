package feed

import (
	"encoding/xml"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/skobkin/magnetico-web/internal/web/trackers"
	"github.com/skobkin/magnetico-web/persistence"
)

// The RSS document. Item identity and structure follow the legacy feed
// (docs/specs/web-v1.md §8.2.1): guid and description are the info hash, title
// is the name, link and enclosure are the magnet URI (with the configured
// trackers and dn), comments is the details page, pubDate is the discovery
// time. XML 1.0 cannot carry control characters, so encoding/xml writes
// U+FFFD for a name that contains one; that is the only normalisation.

const (
	atomNS    = "http://www.w3.org/2005/Atom"
	mimeTorr  = "application/x-bittorrent"
	feedTitle = "Last"
)

type rss struct {
	XMLName xml.Name `xml:"rss"`
	Version string   `xml:"version,attr"`
	AtomNS  string   `xml:"xmlns:atom,attr"`
	Channel channel  `xml:"channel"`
}

type channel struct {
	Title         string   `xml:"title"`
	Description   string   `xml:"description"`
	PubDate       string   `xml:"pubDate"`
	LastBuildDate string   `xml:"lastBuildDate"`
	Generator     string   `xml:"generator"`
	Link          string   `xml:"link"`
	Language      string   `xml:"language"`
	AtomLink      atomLink `xml:"atom:link"`
	Items         []item   `xml:"item"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr"`
}

type item struct {
	Title       string    `xml:"title"`
	Description string    `xml:"description"`
	PubDate     string    `xml:"pubDate"`
	Link        string    `xml:"link"`
	GUID        guid      `xml:"guid"`
	Comments    string    `xml:"comments"`
	Enclosure   enclosure `xml:"enclosure"`
}

type guid struct {
	IsPermaLink string `xml:"isPermaLink,attr"`
	Value       string `xml:",chardata"`
}

type enclosure struct {
	URL    string `xml:"url,attr"`
	Length string `xml:"length,attr"`
	Type   string `xml:"type,attr"`
}

// render builds the document for the rows. base is the public URL (never the
// Host header); selfPath is the feed's own path WITHOUT any query, so the
// credential is never echoed into the document.
func render(rows []persistence.Torrent, urls []string, base *url.URL, selfPath string, now time.Time) ([]byte, error) {
	root := strings.TrimRight(base.String(), "/")
	date := now.UTC().Format(time.RFC1123Z)
	doc := rss{Version: "2.0", AtomNS: atomNS, Channel: channel{
		Title: feedTitle, Description: "Last Last torrents", PubDate: date, LastBuildDate: date,
		Generator: "Magnetico", Link: root + "/", Language: "en-US",
		AtomLink: atomLink{Href: root + selfPath, Rel: "self", Type: "application/rss+xml"},
		Items:    make([]item, 0, len(rows)),
	}}
	for _, t := range rows {
		hash := trackers.InfoHashHex(t.InfoHash)
		name := t.Name
		magnet := trackers.MagnetURI(hash, &name, urls)
		doc.Channel.Items = append(doc.Channel.Items, item{
			Title: t.Name, Description: hash,
			PubDate:   time.Unix(t.DiscoveredOn, 0).UTC().Format(time.RFC1123Z),
			Link:      magnet,
			GUID:      guid{IsPermaLink: "false", Value: hash},
			Comments:  root + "/torrents/" + strconv.FormatInt(int64(t.ID), 10),
			Enclosure: enclosure{URL: magnet, Length: strconv.FormatInt(t.TotalSize, 10), Type: mimeTorr},
		})
	}
	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), append(body, '\n')...), nil
}
