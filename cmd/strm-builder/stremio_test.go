package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestStreamSize(t *testing.T) {
	if got := streamSize(stremioStream{Title: "Movie [gb1 25.0 GB]"}); got != int64(25.0*(1<<30)) {
		t.Errorf("GB parse: %d", got)
	}
	mb := 510.7
	if got := streamSize(stremioStream{Title: "Ep [gb1 510.7 MB]"}); got != int64(mb*(1<<20)) {
		t.Errorf("MB parse: %d", got)
	}
	// structured videoSize wins over the title text
	s := stremioStream{Title: "x [gb1 1.0 GB]"}
	s.BehaviorHints.VideoSize = 999
	if got := streamSize(s); got != 999 {
		t.Errorf("videoSize should win: %d", got)
	}
	if got := streamSize(stremioStream{Title: "no size here"}); got != 0 {
		t.Errorf("missing size: %d", got)
	}
}

func TestSelectVersions(t *testing.T) {
	cands := []candidate{
		{url: "a", res: "1080p", size: 3 << 30},
		{url: "b", res: "2160p", size: 40 << 30},
		{url: "c", res: "1080p", size: 25 << 30}, // largest 1080p
		{url: "d", res: "2160p", size: 54 << 30}, // largest 2160p
		{url: "e", res: "720p", size: 1 << 30},
	}
	// versions=1 -> largest of the best resolution (2160p, 54GB)
	b1 := &bridge{cfg: &serveConfig{versions: 1, pick: "largest"}}
	v := b1.selectVersions(cands)
	if len(v) != 1 || v[0].url != "d" {
		t.Fatalf("versions=1: %+v", v)
	}
	// versions=2 -> best 2160p then best 1080p (one per resolution, quality order)
	b2 := &bridge{cfg: &serveConfig{versions: 2, pick: "largest"}}
	v = b2.selectVersions(cands)
	if len(v) != 2 || v[0].url != "d" || v[1].url != "c" {
		t.Fatalf("versions=2: %+v", v)
	}
	// versions=5 but only 3 distinct resolutions -> 3 results, ordered 2160>1080>720
	b5 := &bridge{cfg: &serveConfig{versions: 5, pick: "largest"}}
	v = b5.selectVersions(cands)
	if len(v) != 3 || v[0].res != "2160p" || v[1].res != "1080p" || v[2].res != "720p" {
		t.Fatalf("versions=5: %+v", v)
	}
}

func TestVersionLabels(t *testing.T) {
	// single version -> no suffix
	if got := versionLabels([]candidate{{res: "1080p"}}); len(got) != 1 || got[0] != "" {
		t.Fatalf("single: %v", got)
	}
	// multiple (distinct resolutions) -> resolution suffixes
	got := versionLabels([]candidate{{res: "2160p"}, {res: "1080p"}, {res: ""}})
	if got[0] != "2160p" || got[1] != "1080p" || got[2] != "SD" {
		t.Fatalf("labels: %v", got)
	}
}

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		`Title: The/Movie?`: "Title The Movie",
		`Spaced   out`:      "Spaced out",
		`trailing dots...`:  "trailing dots",
	}
	for in, want := range cases {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLayoutPaths(t *testing.T) {
	plex := &bridge{cfg: &serveConfig{root: "/out", target: "plex", anime: true, animeDir: "Anime"}}
	if got := plex.moviePath("The Matrix", "1999", "", false); got != filepath.Join("/out", "Movies", "The Matrix (1999)", "The Matrix (1999).strm") {
		t.Errorf("plex moviePath: %q", got)
	}
	if got := plex.episodePath("Breaking Bad", "2008", 1, 1, "", false); got != filepath.Join("/out", "TV Shows", "Breaking Bad (2008)", "Season 01", "Breaking Bad - S01E01.strm") {
		t.Errorf("plex episodePath: %q", got)
	}
	jf := &bridge{cfg: &serveConfig{root: "/out", target: "jellyfin", anime: true, animeDir: "Anime"}}
	if got := jf.episodePath("Breaking Bad", "2008", 2, 5, "", false); got != filepath.Join("/out", "Shows", "Breaking Bad (2008)", "Season 02", "Breaking Bad - S02E05.strm") {
		t.Errorf("jellyfin episodePath: %q", got)
	}

	// Version suffixes land in the same folder so Plex groups them as versions.
	if got := plex.moviePath("The Matrix", "1999", "2160p", false); got != filepath.Join("/out", "Movies", "The Matrix (1999)", "The Matrix (1999) - 2160p.strm") {
		t.Errorf("versioned moviePath: %q", got)
	}
	if got := plex.episodePath("Breaking Bad", "2008", 1, 1, "1080p", false); got != filepath.Join("/out", "TV Shows", "Breaking Bad (2008)", "Season 01", "Breaking Bad - S01E01 - 1080p.strm") {
		t.Errorf("versioned episodePath: %q", got)
	}

	// Anime routes to its own top folder for both movies and series.
	if got := plex.moviePath("Akira", "1988", "", true); got != filepath.Join("/out", "Anime", "Akira (1988)", "Akira (1988).strm") {
		t.Errorf("anime moviePath: %q", got)
	}
	if got := plex.episodePath("Naruto", "2002", 1, 1, "", true); got != filepath.Join("/out", "Anime", "Naruto (2002)", "Season 01", "Naruto - S01E01.strm") {
		t.Errorf("anime episodePath: %q", got)
	}
	// With anime routing disabled, anime falls back to Movies/TV.
	off := &bridge{cfg: &serveConfig{root: "/out", target: "plex", anime: false, animeDir: "Anime"}}
	if got := off.moviePath("Akira", "1988", "", true); got != filepath.Join("/out", "Movies", "Akira (1988)", "Akira (1988).strm") {
		t.Errorf("anime-off moviePath: %q", got)
	}
}

func TestIsAnime(t *testing.T) {
	var kw tmdbMeta
	kw.Keywords.Results = []tmdbKeyword{{ID: 210024}}
	if !kw.isAnime() {
		t.Error("keyword 210024 should classify as anime")
	}
	var jp tmdbMeta
	jp.Genres = []struct {
		ID int `json:"id"`
	}{{ID: 16}}
	jp.OriginalLanguage = "ja"
	if !jp.isAnime() {
		t.Error("Japanese animation should classify as anime")
	}
	var west tmdbMeta
	west.Genres = []struct {
		ID int `json:"id"`
	}{{ID: 16}}
	west.OriginalLanguage = "en"
	if west.isAnime() {
		t.Error("English animation should not classify as anime")
	}
}

func TestBaseID(t *testing.T) {
	tmdbAddon := &bridge{manifest: addonManifest{IDPrefixes: []string{"tmdb:", "tt"}}}
	if got := tmdbAddon.baseID("278", "tt0111161"); got != "tmdb:278" {
		t.Errorf("prefer tmdb when advertised: %q", got)
	}
	ttOnly := &bridge{manifest: addonManifest{IDPrefixes: []string{"tt"}}}
	if got := ttOnly.baseID("278", "tt0111161"); got != "tt0111161" {
		t.Errorf("tt-only addon uses imdb: %q", got)
	}
	unknown := &bridge{manifest: addonManifest{}}
	if got := unknown.baseID("278", ""); got != "tmdb:278" {
		t.Errorf("unknown manifest defaults tmdb: %q", got)
	}
}

func TestHookToReq(t *testing.T) {
	payload := `{
	  "notification_type": "MEDIA_AUTO_APPROVED",
	  "subject": "Breaking Bad (2008)",
	  "media": {"media_type": "tv", "tmdbId": 1396},
	  "extra": [{"name": "Requested Seasons", "value": "1, 2"}]
	}`
	var h seerrHook
	if err := json.Unmarshal([]byte(payload), &h); err != nil {
		t.Fatal(err)
	}
	req, err := hookToReq(h)
	if err != nil {
		t.Fatal(err)
	}
	if req.mediaType != "tv" || req.tmdbID != "1396" {
		t.Fatalf("parsed req = %+v", req)
	}
	if len(req.seasons) != 2 || req.seasons[0] != 1 || req.seasons[1] != 2 {
		t.Fatalf("seasons = %v", req.seasons)
	}

	// tmdbId as a quoted string must parse the same way.
	var h2 seerrHook
	_ = json.Unmarshal([]byte(`{"media":{"media_type":"movie","tmdbId":"278"}}`), &h2)
	if r2, err := hookToReq(h2); err != nil || r2.tmdbID != "278" || r2.mediaType != "movie" {
		t.Fatalf("string tmdbId: %+v err=%v", r2, err)
	}
}
