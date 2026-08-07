package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestStreamSizeAndResolution(t *testing.T) {
	gb := func(f float64) int64 { return int64(f * (1 << 30)) }
	mb := func(f float64) int64 { return int64(f * (1 << 20)) }
	cases := []struct {
		title    string
		wantSize int64
		wantRes  string
	}{
		{"The.Matrix.1999.UHD.BluRay.2160p.TrueHD.Atmos.7.1.DV.HEVC.REMUX-FraMeSToR.mkv [gb1 54.2 GB]", gb(54.2), "2160p"},
		{"Breaking Bad (2008) - S01E01 - Pilot [Bluray-1080p Remux][DTS-HD MA 5.1][AVC]-FraMeSToR.mkv [gb1 11.4 GB]", gb(11.4), "1080p"},
		{"Breaking.Bad.S01E01.(2008).1080p.WEBRIP.HEVC.OPUS2.0.mkv [gb1 510.7 MB]", mb(510.7), "1080p"},
	}
	for _, c := range cases {
		s := stremioStream{Title: c.title}
		if got := streamSize(s); got != c.wantSize {
			t.Errorf("size %q = %d, want %d", c.title, got, c.wantSize)
		}
		if got := streamResolution(s); got != c.wantRes {
			t.Errorf("res %q = %q, want %q", c.title, got, c.wantRes)
		}
	}
	s := stremioStream{Title: "x [gb1 1.0 GB]"}
	s.BehaviorHints.VideoSize = 999
	if got := streamSize(s); got != 999 {
		t.Errorf("videoSize should win: %d", got)
	}
}

func TestSelectVersions(t *testing.T) {
	streams := []stremioStream{
		{Title: "The.Matrix.1999.UHD.BluRay.2160p.TrueHD.Atmos.7.1.DV.HEVC.REMUX-FraMeSToR.mkv [gb1 54.2 GB]", URL: "uhd-dv"},
		{Title: "The.Matrix.1999.UHD.Dolby.Vision.2160p.BluRay.Remux.HEVC.mkv [gb1 48.7 GB]", URL: "uhd"},
		{Title: "The.Matrix.1999.Remastered.1080p.BluRay.Remux.TrueHD.Atmos.7.1.mkv [gb1 30.5 GB]", URL: "hd-remux"},
		{Title: "The.Matrix.1999.1080p.BluRay.DDP5.1.x265.10bit-LAMA.mkv [gb1 3.0 GB]", URL: "hd-x265"},
	}
	var cands []candidate
	for _, s := range streams {
		cands = append(cands, candidate{url: s.URL, size: streamSize(s), res: streamResolution(s)})
	}
	b1 := &bridge{cfg: &serveConfig{versions: 1, pick: "largest"}}
	if v := b1.selectVersions(cands); len(v) != 1 || v[0].url != "uhd-dv" {
		t.Fatalf("versions=1: %+v", v)
	}
	b2 := &bridge{cfg: &serveConfig{versions: 2, pick: "largest"}}
	if v := b2.selectVersions(cands); len(v) != 2 || v[0].url != "uhd-dv" || v[1].url != "hd-remux" {
		t.Fatalf("versions=2: %+v", v)
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
	plex := &bridge{cfg: &serveConfig{root: "/out", target: "plex", anime: true, animeDir: "Anime", animeMoviesDir: "Anime Movies"}}

	inception := "https://host/movies/Inception (2010)/Inception.2010.UHD.BluRay.2160p.DTS-HD.MA.5.1.DV.HEVC.HYBRID.REMUX-FraMeSToR.mkv"
	if got := plex.moviePath("Inception", "2010", "27205", releaseName(inception), false); got != filepath.Join("/out", "Movies", "Inception (2010) {tmdb-27205}", "Inception.2010.UHD.BluRay.2160p.DTS-HD.MA.5.1.DV.HEVC.HYBRID.REMUX-FraMeSToR.strm") {
		t.Errorf("movie: %q", got)
	}

	bb := "https://host/tvs/Breaking Bad/Season 1/Breaking Bad (2008) - S01E01 - Pilot [Bluray-1080p Remux][DTS-HD MA 5.1][AVC]-FraMeSToR.mkv"
	if got := plex.episodePath("Breaking Bad", "2008", "81189", "1396", 1, releaseName(bb), false); got != filepath.Join("/out", "TV Shows", "Breaking Bad (2008) {tvdb-81189}", "Season 01", "Breaking Bad (2008) - S01E01 - Pilot [Bluray-1080p Remux][DTS-HD MA 5.1][AVC]-FraMeSToR.strm") {
		t.Errorf("episode: %q", got)
	}

	if got := plex.episodePath("Breaking Bad", "2008", "", "1396", 1, releaseName(bb), false); got != filepath.Join("/out", "TV Shows", "Breaking Bad (2008) {tmdb-1396}", "Season 01", "Breaking Bad (2008) - S01E01 - Pilot [Bluray-1080p Remux][DTS-HD MA 5.1][AVC]-FraMeSToR.strm") {
		t.Errorf("tmdb-fallback episode: %q", got)
	}

	saga := "https://host/movies/Saga of Tanya the Evil - The Movie (2019)/Saga of Tanya the Evil The Movie (2019) {imdb-tt9507276} [Bluray-1080p][FLAC 5.1][x265]-Vodes.mkv"
	if got := plex.moviePath("Saga of Tanya the Evil: The Movie", "2019", "553839", releaseName(saga), true); got != filepath.Join("/out", "Anime Movies", "Saga of Tanya the Evil The Movie (2019) {tmdb-553839}", "Saga of Tanya the Evil The Movie (2019) {imdb-tt9507276} [Bluray-1080p][FLAC 5.1][x265]-Vodes.strm") {
		t.Errorf("anime movie: %q", got)
	}
	boruto := "https://host/tvs/Boruto - Naruto Next Generations/Season 1/Boruto - Naruto Next Generations (2017) - S01E01 - Boruto Uzumaki! [Bluray-1080p Remux][DTS-HD MA 2.0][AVC]-ZR.mkv"
	if got := plex.episodePath("Boruto: Naruto Next Generations", "2017", "323105", "72636", 1, releaseName(boruto), true); got != filepath.Join("/out", "Anime", "Boruto Naruto Next Generations (2017) {tvdb-323105}", "Season 01", "Boruto - Naruto Next Generations (2017) - S01E01 - Boruto Uzumaki! [Bluray-1080p Remux][DTS-HD MA 2.0][AVC]-ZR.strm") {
		t.Errorf("anime episode: %q", got)
	}

	inMovies := &bridge{cfg: &serveConfig{root: "/out", target: "plex", anime: true, animeDir: "Anime", animeMoviesDir: ""}}
	if got := inMovies.moviePath("Akira", "1988", "149", releaseName("https://host/x/Akira.1988.1080p.BluRay.x264.mkv"), true); got != filepath.Join("/out", "Movies", "Akira (1988) {tmdb-149}", "Akira.1988.1080p.BluRay.x264.strm") {
		t.Errorf("anime-movies-in-movies: %q", got)
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

func TestUpdateSeerr(t *testing.T) {
	var postPath, sentKey string
	var postBody map[string]bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sentKey = r.Header.Get("X-Api-Key")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/request/42":
			io.WriteString(w, `{"is4k":true,"media":{"id":99}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/media/99/partial":
			postPath = r.URL.Path
			json.NewDecoder(r.Body).Decode(&postBody)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	b := &bridge{cfg: &serveConfig{seerrURL: srv.URL, seerrKey: "secret", timeout: 5 * time.Second}, client: srv.Client()}
	b.updateSeerr("42", "partial")
	if sentKey != "secret" {
		t.Errorf("X-Api-Key = %q", sentKey)
	}
	if postPath != "/api/v1/media/99/partial" {
		t.Fatalf("status not routed; got %q", postPath)
	}
	if !postBody["is4k"] {
		t.Errorf("is4k from the request not forwarded: %v", postBody)
	}

	(&bridge{cfg: &serveConfig{}, client: srv.Client()}).updateSeerr("42", "available")
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

	var h2 seerrHook
	_ = json.Unmarshal([]byte(`{"media":{"media_type":"movie","tmdbId":"278"}}`), &h2)
	if r2, err := hookToReq(h2); err != nil || r2.tmdbID != "278" || r2.mediaType != "movie" {
		t.Fatalf("string tmdbId: %+v err=%v", r2, err)
	}
}
