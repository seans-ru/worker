// Package media classifies torrent files into video / audio / subtitle / other
// and parses series episode numbers and dubbing/voice-track hints from names.
package media

import (
	"path"
	"regexp"
	"strconv"
	"strings"
)

type FileKind string

const (
	KindVideo    FileKind = "video"
	KindAudio    FileKind = "audio"
	KindSubtitle FileKind = "subtitle"
	KindOther    FileKind = "other"
)

// TrackLanguage / TrackFlags describe external audio or embedded multi-audio naming.
type TrackLanguage string

const (
	LangUnknown TrackLanguage = ""
	LangRu      TrackLanguage = "ru"
	LangEn      TrackLanguage = "en"
	LangUk      TrackLanguage = "uk"
	LangDe      TrackLanguage = "de"
	LangFr      TrackLanguage = "fr"
	LangEs      TrackLanguage = "es"
	LangIt      TrackLanguage = "it"
	LangJa      TrackLanguage = "ja"
	LangKo      TrackLanguage = "ko"
	LangZh      TrackLanguage = "zh"
)

const (
	FlagDub        = "dub"         // дубляж / DUB / Dubbed
	FlagVoice      = "voice"       // многоголосый / VO / Voice-over
	FlagOneVoice   = "one_voice"   // одноголосый
	FlagOriginal   = "original"    // оригинал / ORIG / Original
	FlagSubbed     = "sub"         // субтитры-оригинал
	FlagDualAudio  = "dual_audio"  // Multi / Dual / 2xRus
	FlagStudioDub  = "studio_dub"  // Лостфилм, Амедиа, ...
	FlagSecondary  = "secondary"   // sample / trailer / extras
)

var (
	videoExt = map[string]bool{
		".mkv": true, ".mp4": true, ".m4v": true, ".avi": true,
		".mov": true, ".ts": true, ".m2ts": true, ".webm": true, ".wmv": true, ".mpg": true, ".mpeg": true,
	}
	audioExt = map[string]bool{
		".ac3": true, ".eac3": true, ".dts": true, ".m4a": true, ".aac": true,
		".flac": true, ".wav": true, ".mka": true, ".opus": true, ".mp3": true,
	}
	subExt = map[string]bool{
		".srt": true, ".ass": true, ".ssa": true, ".sub": true, ".vtt": true,
		".idx": true, ".sup": true, ".smi": true,
	}

	reSeasonEp = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(?:s(\d{1,2})\s*[._-]?\s*e(\d{1,3}(?:\s*-\s*e?\d{1,3})?|e\d{1,3}(?:\s*e\d{1,3})*))`)
	reSxxExx   = regexp.MustCompile(`(?i)\bs(\d{1,2})\s*e(\d{1,3})(?:\s*[-–]\s*e?(\d{1,3}))?`)
	reNxNN     = regexp.MustCompile(`(?:^|[^\d])(\d{1,2})x(\d{2,3})(?:[^\d]|$)`)
	reEpOnly   = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(?:ep?|episode|ep\.|серия|серия\.|сер\.?)(?:\s|._-)*(\d{1,3})(?:[^\d]|$)`)
	reBracketEp = regexp.MustCompile(`\[` + `(?:` + `(?:EP?|EPISODE|С)\s*` + `)?` + `(\d{1,3})` + `(?:\s*v\d)?` + `\]`)
	reSeasonWord = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(?:season|сезон)\s*(\d{1,2})(?:[^\d]|$)`)
	reYear       = regexp.MustCompile(`(?:^|[^\d])((?:19|20)\d{2})(?:[^\d]|$)`)

	reLangToken = regexp.MustCompile(`(?i)(?:^|[._\-\[\] ])(rus|ru|eng|en|ukr|ua|uk|ger|de|fre|fr|spa|es|ita|it|jpn|ja|kor|ko|chi|zh|chn)(?:[._\-\[\] ]|$)`)
)

// FileMeta is the classification of a single torrent path.
type FileMeta struct {
	Path     string
	Name     string // base name
	Ext      string
	Kind     FileKind
	Size     int64
	Index    int

	// Series
	Season       int
	Episode      int
	EpisodeEnd   int // multi-episode end; 0 if single
	IsSeries     bool
	IsMovie      bool
	MovieYear    int

	// Audio / track naming (works for external audio and for dual-audio video names)
	Language TrackLanguage
	Flags    []string
}

// EpisodeKey groups files belonging to the same episode (or movie).
type EpisodeKey struct {
	Season  int
	Episode int // start episode; 0 for movie
	MovieYear int
}

// EpisodeGroup is one playable item (episode or movie) with its assets.
type EpisodeGroup struct {
	Key EpisodeKey

	// Primary is the preferred video file (largest video for that episode).
	Primary *FileMeta
	// Videos are all video files for that episode (different releases / dubbing cuts).
	Videos []*FileMeta
	// ExternalAudio are separate audio files (ac3/m4a/...) for that item.
	ExternalAudio []*FileMeta
	// Subtitles are external subtitle files.
	Subtitles []*FileMeta
}

// MediaCatalog is the parsed view of a torrent's file list.
type MediaCatalog struct {
	Files    []*FileMeta
	Groups   []*EpisodeGroup
	// Unknown are non-media files kept for upload completeness.
	Unknown []*FileMeta
}

// ParsePath classifies a single torrent-relative path.
func ParsePath(p string, size int64, index int) *FileMeta {
	p = strings.ReplaceAll(p, "\\", "/")
	base := path.Base(p)
	ext := strings.ToLower(path.Ext(base))
	name := strings.TrimSuffix(base, path.Ext(base))

	m := &FileMeta{
		Path:  p,
		Name:  base,
		Ext:   ext,
		Size:  size,
		Index: index,
		Kind:  KindOther,
	}

	switch {
	case videoExt[ext]:
		m.Kind = KindVideo
	case audioExt[ext]:
		m.Kind = KindAudio
	case subExt[ext]:
		m.Kind = KindSubtitle
	default:
		m.Kind = KindOther
		return m
	}

	// Series / movie detection on full relative path (folder season hints included).
	full := p
	if s, e, end, ok := parseEpisode(full); ok {
		m.IsSeries = true
		m.Season = s
		m.Episode = e
		m.EpisodeEnd = end
	} else if y := parseYear(name); y > 0 && m.Kind == KindVideo {
		m.IsMovie = true
		m.MovieYear = y
	} else if m.Kind == KindVideo {
		// Video without year/episode — treat as movie-like single item.
		m.IsMovie = true
	}

	m.Language, m.Flags = parseLanguageAndFlags(base)
	return m
}

func parseEpisode(s string) (season, episode, end int, ok bool) {
	// SxxEyy / SxxEyy-Ezz / SxxEyyEzz
	if loc := reSxxExx.FindStringSubmatch(s); loc != nil {
		season, _ = strconv.Atoi(loc[1])
		episode, _ = strconv.Atoi(loc[2])
		if loc[3] != "" {
			e := strings.TrimLeft(loc[3], "eE-– ")
			end, _ = strconv.Atoi(e)
			if end < episode {
				end = 0
			}
		}
		// S01E01E02 style packed into group 2 sometimes
		if episode == 0 {
			return 0, 0, 0, false
		}
		if season == 0 {
			season = 1
		}
		return season, episode, end, true
	}

	// 1x02
	if loc := reNxNN.FindStringSubmatch(s); loc != nil {
		season, _ = strconv.Atoi(loc[1])
		episode, _ = strconv.Atoi(loc[2])
		if episode > 0 {
			if season == 0 {
				season = 1
			}
			return season, episode, 0, true
		}
	}

	// Episode 02 / серия 02 — season from "Season N" folder or default 1
	if loc := reEpOnly.FindStringSubmatch(s); loc != nil {
		episode, _ = strconv.Atoi(loc[1])
		if episode == 0 {
			return 0, 0, 0, false
		}
		season = 1
		if loc2 := reSeasonWord.FindStringSubmatch(s); loc2 != nil {
			season, _ = strconv.Atoi(loc2[1])
		}
		return season, episode, 0, true
	}

	// [02] anime-style — only if path looks episodic (has Season/TV-like or many files; caller uses size)
	if loc := reBracketEp.FindStringSubmatch(s); loc != nil {
		episode, _ = strconv.Atoi(loc[1])
		// Avoid treating [1080p] as episode: 1080 > 200 already filtered by 1-3 digits max 999,
		// but [1080p] has 4 digits so won't match. [24] could be bitrate — require episode <= 200
		// and prefer when "season" word present or filename has no quality token near it.
		if episode >= 1 && episode <= 200 && !reYear.MatchString(loc[0]) {
			season = 1
			if loc2 := reSeasonWord.FindStringSubmatch(s); loc2 != nil {
				season, _ = strconv.Atoi(loc2[1])
			}
			return season, episode, 0, true
		}
	}

	return 0, 0, 0, false
}

func parseYear(name string) int {
	// Prefer the last year-like token (release year is usually last before quality).
	all := reYear.FindAllStringSubmatch(name, -1)
	if len(all) == 0 {
		return 0
	}
	y, _ := strconv.Atoi(all[len(all)-1][1])
	if y >= 1920 && y <= 2100 {
		return y
	}
	return 0
}

func parseLanguageAndFlags(name string) (TrackLanguage, []string) {
	var flags []string
	lang := LangUnknown
	lower := strings.ToLower(name)

	// Language tokens
	if loc := reLangToken.FindAllStringSubmatch(name, -1); len(loc) > 0 {
		lang = mapLang(loc[len(loc)-1][1])
	}

	// Dubbing / voice markers (RU + EN)
	switch {
	case containsAny(lower, "дубляж", "dubbed", ".dub.", ".dub ", "_dub", "-dub", "дубл"):
		flags = append(flags, FlagDub)
	case containsAny(lower, "многоголос", "мнгглс", "многоголосый", "vo.", "voice-over", "voiceover", "vo ", "vo_", "лостфилм", "lostfilm", "amedia", "амедиа", "newstudio", "new studio", "dreamcast", "cub"):
		flags = append(flags, FlagVoice, FlagStudioDub)
	case containsAny(lower, "одноголос", "одноголосый", "закадр"):
		flags = append(flags, FlagOneVoice)
	case containsAny(lower, "original", "оригинал", ".orig.", "org.", "truefrench"):
		flags = append(flags, FlagOriginal)
	}

	if containsAny(lower, "multi", "dual", "2xrus", "2xeng", "rus+eng", "eng+rus", "2aud", "dual.audio") {
		flags = append(flags, FlagDualAudio)
	}
	if containsAny(lower, "sample", "trailer", "extras", "реклама") {
		flags = append(flags, FlagSecondary)
	}

	// If name has explicit rus + dub-like studio, mark dub
	if lang == LangRu && (containsAny(lower, "dub", "дубляж", "дубл")) && !hasFlag(flags, FlagDub) {
		flags = append(flags, FlagDub)
	}

	return lang, flags
}

func mapLang(tok string) TrackLanguage {
	switch strings.ToLower(tok) {
	case "rus", "ru":
		return LangRu
	case "eng", "en":
		return LangEn
	case "ukr", "ua", "uk":
		return LangUk
	case "ger", "de":
		return LangDe
	case "fre", "fr":
		return LangFr
	case "spa", "es":
		return LangEs
	case "ita", "it":
		return LangIt
	case "jpn", "ja":
		return LangJa
	case "kor", "ko":
		return LangKo
	case "chi", "zh", "chn":
		return LangZh
	}
	return LangUnknown
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func hasFlag(flags []string, f string) bool {
	for _, x := range flags {
		if x == f {
			return true
		}
	}
	return false
}

// Classify parses all files and groups them into playable episodes/movies.
// primaryFilter, if non-nil, restricts which paths are considered selected
// (task file_paths). When empty, all media files are considered.
func Classify(paths []string, sizes []int64, primaryFilter []string) *MediaCatalog {
	cat := &MediaCatalog{}
	want := make(map[string]bool, len(primaryFilter))
	for _, p := range primaryFilter {
		want[normalizePath(p)] = true
	}

	byKey := map[EpisodeKey]*EpisodeGroup{}
	var order []EpisodeKey

	for i, p := range paths {
		var size int64
		if i < len(sizes) {
			size = sizes[i]
		}
		m := ParsePath(p, size, i)
		cat.Files = append(cat.Files, m)

		if m.Kind == KindOther {
			if len(want) == 0 || want[normalizePath(m.Path)] {
				cat.Unknown = append(cat.Unknown, m)
			}
			continue
		}

		if len(want) > 0 && !want[normalizePath(m.Path)] {
			// Keep unmatched extras out of groups but still classified.
			continue
		}

		if m.Kind == KindVideo {
			key := EpisodeKey{Season: m.Season, Episode: m.Episode, MovieYear: m.MovieYear}
			if m.IsMovie {
				key.Episode = 0
			}
			g, ok := byKey[key]
			if !ok {
				g = &EpisodeGroup{Key: key}
				byKey[key] = g
				order = append(order, key)
			}
			g.Videos = append(g.Videos, m)
			continue
		}

		// Attach audio/subs to the matching episode if we can, else last group / movie.
		key := EpisodeKey{Season: m.Season, Episode: m.Episode, MovieYear: m.MovieYear}
		if !m.IsSeries {
			// try match by name prefix against existing groups
			key = matchKeyForSidecar(m, byKey, order)
		}
		g, ok := byKey[key]
		if !ok {
			// create a sidecar-only group under detected key or zero key
			g = &EpisodeGroup{Key: key}
			byKey[key] = g
			order = append(order, key)
		}
		if m.Kind == KindAudio {
			g.ExternalAudio = append(g.ExternalAudio, m)
		} else {
			g.Subtitles = append(g.Subtitles, m)
		}
	}

	for _, key := range order {
		g := byKey[key]
		g.Primary = pickPrimaryVideo(g.Videos)
		cat.Groups = append(cat.Groups, g)
	}
	return cat
}

func matchKeyForSidecar(m *FileMeta, byKey map[EpisodeKey]*EpisodeGroup, order []EpisodeKey) EpisodeKey {
	base := strings.ToLower(m.Name)
	// strip extension already done in Name; strip common lang suffixes for matching
	for _, key := range order {
		g := byKey[key]
		if g.Primary == nil {
			continue
		}
		pn := strings.ToLower(g.Primary.Name)
		// longest common-ish: sidecar name starts with primary name or vice versa
		if strings.HasPrefix(base, trimReleaseTags(pn)) || strings.HasPrefix(trimReleaseTags(pn), trimReleaseTags(base)) {
			return key
		}
		// also compare after removing language/flag tokens
		if similarStem(trimReleaseTags(base), trimReleaseTags(pn)) {
			return key
		}
	}
	if len(order) > 0 {
		return order[len(order)-1]
	}
	return EpisodeKey{}
}

func trimReleaseTags(s string) string {
	// drop quality / source / audio tags after the title for fuzzy sidecar matching
	s = strings.ToLower(s)
	for _, tag := range []string{"2160p", "1080p", "720p", "480p", "2160", "1080", "720",
		"web-dl", "webdl", "web", "bluray", "bdrip", "brrip", "hdtv", "remux", "hevc", "h264", "h265", "x264", "x265", "avc", "aac", "ac3", "eac3", "dts", "hdr", "hdr10", "sdr", "10bit", "8bit"} {
		if idx := strings.Index(s, tag); idx > 8 {
			s = s[:idx]
			break
		}
	}
	return strings.Trim(s, " ._-")
}

func similarStem(a, b string) bool {
	if a == b {
		return true
	}
	// if one contains the other after cleanup
	if len(a) >= 4 && len(b) >= 4 {
		return strings.Contains(a, b) || strings.Contains(b, a)
	}
	return false
}

func pickPrimaryVideo(videos []*FileMeta) *FileMeta {
	if len(videos) == 0 {
		return nil
	}
	best := videos[0]
	for _, v := range videos[1:] {
		if v.Size > best.Size {
			best = v
		}
	}
	// Prefer non-secondary
	for _, v := range videos {
		if !hasFlag(v.Flags, FlagSecondary) && v.Size >= best.Size {
			best = v
		}
	}
	return best
}

func normalizePath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.TrimPrefix(p, "/")
	return p
}

// SelectDownloadPriorities returns torrent file indices to enable (priority>0).
// Primary videos first, then external audio, then subs; samples skipped unless requested.
func (c *MediaCatalog) SelectDownloadPriorities(includeAll bool) (selected []int, primary []int) {
	for _, g := range c.Groups {
		if g.Primary != nil {
			primary = append(primary, g.Primary.Index)
			selected = append(selected, g.Primary.Index)
		}
		for _, v := range g.Videos {
			if g.Primary != nil && v.Index == g.Primary.Index {
				continue
			}
			if hasFlag(v.Flags, FlagSecondary) {
				continue
			}
			selected = append(selected, v.Index)
		}
		for _, a := range g.ExternalAudio {
			selected = append(selected, a.Index)
		}
		for _, s := range g.Subtitles {
			selected = append(selected, s.Index)
		}
	}
	if includeAll {
		for _, f := range c.Files {
			if f.Kind == KindOther && !hasFlag(f.Flags, FlagSecondary) {
				selected = append(selected, f.Index)
			}
		}
	}
	return selected, primary
}

// FirstPlayable returns the first group that has a primary video (stream target).
func (c *MediaCatalog) FirstPlayable() *EpisodeGroup {
	for _, g := range c.Groups {
		if g.Primary != nil {
			return g
		}
	}
	return nil
}
