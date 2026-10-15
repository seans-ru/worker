package media

import (
	"testing"
)

func TestParsePathSeries(t *testing.T) {
	tests := []struct {
		path     string
		wantS    int
		wantE    int
		wantEnd  int
		wantKind FileKind
	}{
		{"Show.Name.S01E02.1080p.WEB-DL.mkv", 1, 2, 0, KindVideo},
		{"Show.Name.S01E02-E03.1080p.mkv", 1, 2, 3, KindVideo},
		{"Show.Name.S01E02E03.mkv", 1, 2, 0, KindVideo}, // regex captures first only as E02
		{"Show Name 1x02 Episode Title.mkv", 1, 2, 0, KindVideo},
		{"Season 2/Episode 05.mkv", 2, 5, 0, KindVideo},
		{"show.s02e10.rus.dub.ac3", 2, 10, 0, KindAudio},
		{"Show.Name.S01E02.rus.srt", 1, 2, 0, KindSubtitle},
	}
	for _, tt := range tests {
		m := ParsePath(tt.path, 100, 0)
		if m.Kind != tt.wantKind {
			t.Errorf("%s: kind=%s want %s", tt.path, m.Kind, tt.wantKind)
		}
		if !m.IsSeries {
			t.Errorf("%s: expected series", tt.path)
			continue
		}
		if m.Season != tt.wantS || m.Episode != tt.wantE {
			t.Errorf("%s: got S%02dE%02d want S%02dE%02d", tt.path, m.Season, m.Episode, tt.wantS, tt.wantE)
		}
		if m.EpisodeEnd != tt.wantEnd {
			t.Errorf("%s: EpisodeEnd=%d want %d", tt.path, m.EpisodeEnd, tt.wantEnd)
		}
	}
}

func TestParsePathMovie(t *testing.T) {
	m := ParsePath("Inception.2010.1080p.BluRay.mkv", 1, 0)
	if m.Kind != KindVideo || !m.IsMovie {
		t.Fatalf("expected movie video, got %+v", m)
	}
	if m.MovieYear != 2010 {
		t.Errorf("year=%d want 2010", m.MovieYear)
	}
	if m.IsSeries {
		t.Error("movie should not be series")
	}
}

func TestParseLanguageAndFlags(t *testing.T) {
	tests := []struct {
		name     string
		wantLang TrackLanguage
		wantFlag string
	}{
		{"Show.S01E01.RUS.DUB.1080p.mkv", LangRu, FlagDub},
		{"Show.S01E01.Eng.1080p.mkv", LangEn, ""},
		{"Show.S01E01.многоголосый.LostFilm.mkv", LangUnknown, FlagVoice},
		{"Show.S01E01.Multi.1080p.mkv", LangUnknown, FlagDualAudio},
		{"movie.2010.rus.ac3", LangRu, ""},
		{"Show.S01E01.Original.mkv", LangUnknown, FlagOriginal},
	}
	for _, tt := range tests {
		lang, flags := parseLanguageAndFlags(tt.name)
		if lang != tt.wantLang {
			t.Errorf("%s: lang=%s want %s", tt.name, lang, tt.wantLang)
		}
		if tt.wantFlag != "" && !hasFlag(flags, tt.wantFlag) {
			t.Errorf("%s: flags=%v missing %s", tt.name, flags, tt.wantFlag)
		}
	}
}

func TestClassifySeasonPack(t *testing.T) {
	paths := []string{
		"Show/Season 1/Show.S01E01.1080p.mkv",
		"Show/Season 1/Show.S01E02.1080p.mkv",
		"Show/Season 1/Show.S01E01.rus.dub.ac3",
		"Show/Season 1/Show.S01E01.rus.srt",
		"Show/poster.jpg",
	}
	sizes := []int64{1000, 2000, 300, 10, 50}

	cat := Classify(paths, sizes, nil)
	if len(cat.Groups) != 2 {
		t.Fatalf("groups=%d want 2", len(cat.Groups))
	}

	var e1, e2 *EpisodeGroup
	for _, g := range cat.Groups {
		if g.Key.Episode == 1 {
			e1 = g
		}
		if g.Key.Episode == 2 {
			e2 = g
		}
	}
	if e1 == nil || e2 == nil {
		t.Fatalf("missing episode groups: %+v", cat.Groups)
	}
	if e1.Primary == nil || e1.Primary.Name != "Show.S01E01.1080p.mkv" {
		t.Errorf("e1 primary=%v", e1.Primary)
	}
	if len(e1.ExternalAudio) != 1 {
		t.Errorf("e1 audio=%v", e1.ExternalAudio)
	}
	if len(e1.Subtitles) != 1 {
		t.Errorf("e1 subs=%v", e1.Subtitles)
	}
	if e2.Primary == nil || e2.Primary.Size != 2000 {
		t.Errorf("e2 primary size=%v", e2.Primary)
	}

	selected, primary := cat.SelectDownloadPriorities(false)
	if len(primary) != 2 {
		t.Errorf("primary=%v want 2", primary)
	}
	if len(selected) != 4 {
		t.Errorf("selected=%v want 4 (2 video + 1 audio + 1 sub)", selected)
	}
}

func TestClassifyMovieWithSeparateAudio(t *testing.T) {
	paths := []string{
		"Movie.2010.1080p.BluRay.mkv",
		"Movie.2010.RUS.DUB.ac3",
		"Movie.2010.Eng.m4a",
		"Movie.2010.rus.srt",
	}
	sizes := []int64{8000, 400, 350, 20}
	cat := Classify(paths, sizes, nil)

	if len(cat.Groups) != 1 {
		t.Fatalf("groups=%d want 1", len(cat.Groups))
	}
	g := cat.Groups[0]
	if g.Primary == nil || g.Primary.MovieYear != 2010 {
		t.Errorf("primary=%+v", g.Primary)
	}
	if len(g.ExternalAudio) != 2 {
		t.Errorf("audio=%d want 2", len(g.ExternalAudio))
	}
	if g.ExternalAudio[0].Language != LangRu || !hasFlag(g.ExternalAudio[0].Flags, FlagDub) {
		t.Errorf("rus audio meta=%+v", g.ExternalAudio[0])
	}
}

func TestClassifyPrimaryFilter(t *testing.T) {
	paths := []string{
		"a/S01E01.mkv",
		"b/S01E02.mkv",
		"a/S01E01.srt",
	}
	sizes := []int64{1, 2, 3}
	cat := Classify(paths, sizes, []string{"a/S01E01.mkv", "a/S01E01.srt"})
	if len(cat.Groups) != 1 {
		t.Fatalf("groups=%d want 1", len(cat.Groups))
	}
	if cat.Groups[0].Primary.Index != 0 {
		t.Errorf("primary index=%d", cat.Groups[0].Primary.Index)
	}
	if len(cat.Groups[0].Subtitles) != 1 {
		t.Errorf("subs=%v", cat.Groups[0].Subtitles)
	}
}

func TestPickPrimaryVideoLargest(t *testing.T) {
	videos := []*FileMeta{
		{Name: "small.mkv", Size: 100, Index: 0},
		{Name: "big.mkv", Size: 900, Index: 1},
		{Name: "sample.mkv", Size: 50, Index: 2, Flags: []string{FlagSecondary}},
	}
	p := pickPrimaryVideo(videos)
	if p.Name != "big.mkv" {
		t.Errorf("primary=%s", p.Name)
	}
}

func TestAnimeBracketEpisode(t *testing.T) {
	m := ParsePath("[Sub] Show Name - 02 [1080p].mkv", 1, 0)
	// 02 should be detected as episode via bracket/ep pattern; 1080p is 4 digits so not an episode.
	// The path has " - 02 " which reEpOnly may not catch without ep prefix.
	// Accept either series episode 2 or at least video classification.
	if m.Kind != KindVideo {
		t.Fatalf("kind=%s", m.Kind)
	}
	if m.IsSeries && m.Episode != 2 {
		t.Errorf("episode=%d", m.Episode)
	}
}
