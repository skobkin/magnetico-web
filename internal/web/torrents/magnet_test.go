package torrents

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
)

type staticTrackers []string

func (s staticTrackers) URLs() []string { return s }

func TestMagnetLinksCarryTrackersInOrderAndDetailsShowTheShortLink(t *testing.T) {
	pu, _ := url.Parse("https://magnet.example.org")
	tr := staticTrackers{"udp://tracker.one:1337/announce", "http://tracker.two:80/announce"}
	e := newEnv(t, func(o *envOpts) { o.trackers = tr; o.publicBase = pu })
	id := e.seed("Ubuntu 24.04", 1234, 1_700_000_000, nil)
	c := e.signedIn()

	wantTr := "&amp;tr=udp%3A%2F%2Ftracker.one%3A1337%2Fannounce&amp;tr=http%3A%2F%2Ftracker.two%3A80%2Fannounce"
	c.get("/torrents")
	if !strings.Contains(c.body, "dn=Ubuntu&#43;24.04"+wantTr) {
		t.Fatalf("list magnet lacks the trackers:\n%s", c.body)
	}
	c.get(fmt.Sprintf("/torrents/%d", id))
	if !strings.Contains(c.body, "dn=Ubuntu&#43;24.04"+wantTr) {
		t.Fatalf("details magnet lacks the trackers:\n%s", c.body)
	}
	var hash string
	if err := e.pool.QueryRow(tctx(t), `SELECT encode(info_hash,'hex') FROM magneticod.torrents WHERE id = $1`, id).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.body, `data-copy="https://magnet.example.org/magnet/`+hash+`"`) || !strings.Contains(c.body, "Copy short link") {
		t.Fatalf("details lacks the short link:\n%s", c.body)
	}
}

func TestWithoutTrackersOrPublicURLMagnetsAreUnchangedAndNoShortLink(t *testing.T) {
	e := newEnv(t)
	id := e.seed("Plain", 10, 1_700_000_000, nil)
	c := e.signedIn()
	c.get(fmt.Sprintf("/torrents/%d", id))
	if strings.Contains(c.body, "tr=") || strings.Contains(c.body, "Copy short link") {
		t.Fatalf("unexpected tracker or short link:\n%s", c.body)
	}
}
