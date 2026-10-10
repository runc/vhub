package douyin

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Candidate is one downloadable media URL plus the headers it needs.
type Candidate struct {
	URL     string
	Headers http.Header
}

// Quality selects the bit-rate tier of a video ("highest" / "lowest").
type Quality string

const (
	QualityHighest Quality = "highest"
	QualityLowest  Quality = "lowest"
)

var watermarkHints = []string{
	"watermark=1",
	"owner_watermark",
	"watermark_image",
}

func isWatermarkedMediaURL(u string) bool {
	l := strings.ToLower(u)
	for _, h := range watermarkHints {
		if strings.Contains(l, h) {
			return true
		}
	}
	return false
}

func mapGet(m map[string]any, keys ...string) map[string]any {
	cur := m
	for _, k := range keys {
		v, ok := cur[k].(map[string]any)
		if !ok {
			return nil
		}
		cur = v
	}
	return cur
}

func mapList(m map[string]any, key string) []any {
	v, _ := m[key].([]any)
	return v
}

func mapStr(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func mapFloat(m map[string]any, key string) float64 {
	f, _ := m[key].(float64)
	return f
}

func urlList(m map[string]any) []string {
	var out []string
	for _, key := range []string{"url_list", "urlList"} {
		for _, v := range mapList(m, key) {
			if s, ok := v.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return out
}

// resolution returns the short edge of a play_addr, used to rank bit-rate
// entries like _resolution_metrics of the reference implementation.
func playAddrShortEdge(m map[string]any) int {
	w, h := mapFloat(m, "width"), mapFloat(m, "height")
	if w <= 0 || h <= 0 {
		return 0
	}
	if w < h {
		return int(w)
	}
	return int(h)
}

// pickPlayAddr selects a play_addr by quality from video.bit_rate, falling
// back to video.play_addr (mirrors _pick_play_addr_by_quality).
func pickPlayAddr(video map[string]any, quality Quality) map[string]any {
	type entry struct {
		br        float64
		shortEdge int
		pixels    float64
		pa        map[string]any
	}
	var entries []entry
	for _, v := range mapList(video, "bit_rate") {
		em, ok := v.(map[string]any)
		if !ok {
			continue
		}
		pa, ok := em["play_addr"].(map[string]any)
		if !ok || len(urlList(pa)) == 0 {
			continue
		}
		se := playAddrShortEdge(pa)
		entries = append(entries, entry{mapFloat(em, "bit_rate"), se, float64(se) * float64(se), pa})
	}
	if len(entries) == 0 {
		if pa, ok := video["play_addr"].(map[string]any); ok {
			return pa
		}
		return nil
	}
	best := entries[0]
	for _, e := range entries[1:] {
		if quality == QualityLowest {
			if e.shortEdge < best.shortEdge || (e.shortEdge == best.shortEdge && e.br < best.br) {
				best = e
			}
		} else if e.shortEdge > best.shortEdge || (e.shortEdge == best.shortEdge && e.pixels > best.pixels) {
			best = e
		}
	}
	return best.pa
}

func isPaidContent(aweme map[string]any) bool {
	if v, ok := aweme["is_paid_content"].(bool); ok && v {
		return true
	}
	if f := mapFloat(aweme, "is_paid_content"); f != 0 {
		return true
	}
	return false
}

// pickSourceURI mirrors _pick_source_uri: paid content must not fall back to
// download_addr (that URL serves the CENC-encrypted full film).
func pickSourceURI(video, playAddr map[string]any, paid bool) string {
	if uri := mapStr(playAddr, "uri"); uri != "" {
		return uri
	}
	if uri := mapStr(video, "vid"); uri != "" {
		return uri
	}
	if !paid {
		if da, ok := video["download_addr"].(map[string]any); ok {
			return mapStr(da, "uri")
		}
	}
	return ""
}

// VideoCandidates returns the ordered download candidates for an aweme,
// ported from _build_video_url_candidates:
//  1. direct CDN mirrors (watermark=0 first),
//  2. the douyin.com /aweme/v1/play/ endpoint (signed),
//  3. a constructed signed play URL from the track uri,
//  4. the first watermarked URL as a last resort.
func (c *Client) VideoCandidates(aweme map[string]any, quality Quality) []Candidate {
	video, _ := aweme["video"].(map[string]any)
	if video == nil {
		return nil
	}
	pa := pickPlayAddr(video, quality)
	if pa == nil {
		pa = map[string]any{}
	}
	urls := urlList(pa)
	// watermark=0 first, stable
	var clean, other []string
	for _, u := range urls {
		if strings.Contains(u, "watermark=0") {
			clean = append(clean, u)
		} else {
			other = append(other, u)
		}
	}
	urls = append(clean, other...)

	var out []Candidate
	var playCandidate, watermarkedCandidate *Candidate
	for _, u := range urls {
		wm := isWatermarkedMediaURL(u)
		pu, err := url.Parse(u)
		if err == nil && strings.HasSuffix(pu.Hostname(), "douyin.com") {
			if wm {
				if watermarkedCandidate == nil {
					cand := c.signPlayCandidate(u)
					watermarkedCandidate = &cand
				}
				continue
			}
			if playCandidate == nil {
				cand := c.signPlayCandidate(u)
				playCandidate = &cand
			}
			continue
		}
		if wm {
			if watermarkedCandidate == nil {
				watermarkedCandidate = &Candidate{URL: u, Headers: c.DownloadHeaders("")}
			}
			continue
		}
		out = append(out, Candidate{URL: u, Headers: c.DownloadHeaders("")})
	}
	if playCandidate != nil {
		out = append(out, *playCandidate)
	}
	if len(out) > 0 {
		if watermarkedCandidate != nil {
			out = append(out, *watermarkedCandidate) // last resort
		}
		return out
	}
	// Constructed signed play URL as fallback.
	if uri := pickSourceURI(video, pa, isPaidContent(aweme)); uri != "" {
		params := url.Values{
			"video_id":    {uri},
			"ratio":       {"1080p"},
			"line":        {"0"},
			"is_play_url": {"1"},
			"watermark":   {"0"},
			"source":      {"PackSourceEnum_PUBLISH"},
		}
		signed, ua := c.SignURL(baseURL + "/aweme/v1/play/?" + params.Encode())
		return []Candidate{{URL: signed, Headers: c.DownloadHeaders(ua)}}
	}
	if watermarkedCandidate != nil {
		return []Candidate{*watermarkedCandidate}
	}
	return nil
}

func (c *Client) signPlayCandidate(u string) Candidate {
	if strings.Contains(u, "X-Bogus=") {
		return Candidate{URL: u, Headers: c.DownloadHeaders("")}
	}
	signed, ua := c.SignURL(u)
	return Candidate{URL: signed, Headers: c.DownloadHeaders(ua)}
}

// MediaKind describes what an aweme contains.
type MediaKind string

const (
	KindVideo   MediaKind = "video"
	KindGallery MediaKind = "gallery"
)

var galleryAwemeTypes = map[float64]bool{2: true, 68: true, 150: true}

// DetectMediaKind mirrors _detect_media_type.
func DetectMediaKind(aweme map[string]any) MediaKind {
	if len(galleryImageURLs(aweme)) > 0 {
		return KindGallery
	}
	if galleryAwemeTypes[mapFloat(aweme, "aweme_type")] {
		if video, ok := aweme["video"].(map[string]any); ok && hasVideoSource(video) {
			return KindVideo
		}
		return KindGallery
	}
	return KindVideo
}

func hasVideoSource(video map[string]any) bool {
	if len(urlList(video)) > 0 {
		return true
	}
	for _, k := range []string{"play_addr", "play_addr_h264", "play_addr_265", "download_addr"} {
		if pa, ok := video[k].(map[string]any); ok && len(urlList(pa)) > 0 {
			return true
		}
	}
	return false
}

// GalleryImageURLs returns per-image candidate URL lists, preferring
// no-watermark fields (origin/display) before watermark fallbacks.
func GalleryImageURLs(aweme map[string]any) [][]string {
	return galleryImageURLs(aweme)
}

func galleryImageURLs(aweme map[string]any) [][]string {
	var out [][]string
	for _, v := range mapList(aweme, "images") {
		img, ok := v.(map[string]any)
		if !ok {
			continue
		}
		var urls []string
		// No-watermark fields first, watermark fallbacks last (mirrors the
		// reference implementation's image-source ranking).
		for _, key := range []string{
			"origin_image", "display_image",
			"watermark_free_download_url_list", "download_url_list",
			"owner_watermark_image",
		} {
			src, ok := img[key].(map[string]any)
			if !ok {
				continue
			}
			if urls = urlList(src); len(urls) > 0 {
				break
			}
		}
		if len(urls) == 0 {
			urls = urlList(img)
		}
		if len(urls) > 0 {
			out = append(out, urls)
		}
	}
	return out
}

// AwemeTitle returns a display title for an aweme.
func AwemeTitle(aweme map[string]any) string {
	if d := mapStr(aweme, "desc"); d != "" {
		return d
	}
	if a := mapGet(aweme, "author"); a != nil {
		if n := mapStr(a, "nickname"); n != "" {
			return n + " 的作品"
		}
	}
	return mapStr(aweme, "aweme_id")
}

// AwemeAuthorName returns the author nickname when present.
func AwemeAuthorName(aweme map[string]any) string {
	if a := mapGet(aweme, "author"); a != nil {
		return mapStr(a, "nickname")
	}
	return ""
}

// AwemeAuthorAvatar returns the author's avatar URL when present.
func AwemeAuthorAvatar(aweme map[string]any) string {
	for _, key := range []string{"avatar_medium", "avatar_thumb", "avatar_larger"} {
		if urls := urlList(mapGet(aweme, "author", key)); len(urls) > 0 {
			return urls[0]
		}
	}
	return ""
}

// AwemeID returns the aweme id string.
func AwemeID(aweme map[string]any) string {
	return mapStr(aweme, "aweme_id")
}

// AwemeTime returns the publish time (unix seconds); create_time arrives
// as a string on most endpoints and a number on a few.
func AwemeTime(aweme map[string]any) int64 {
	switch v := aweme["create_time"].(type) {
	case float64:
		return int64(v)
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}

// AwemeCover returns a display cover URL for an aweme: the video poster,
// or the first image of a gallery note.
func AwemeCover(aweme map[string]any) string {
	for _, key := range []string{"cover", "origin_cover", "dynamic_cover"} {
		if urls := urlList(mapGet(aweme, "video", key)); len(urls) > 0 {
			return urls[0]
		}
	}
	if images := mapList(aweme, "images"); len(images) > 0 {
		if img, ok := images[0].(map[string]any); ok {
			if urls := urlList(img); len(urls) > 0 {
				return urls[0]
			}
		}
	}
	return ""
}

// MixInfo summarizes one entry of a profile's mix/list for the pick UI.
type MixInfo struct {
	ID    string
	Name  string
	Count int
	Cover string
}

// MixInfoOf extracts a collection's summary from a mix/list entry.
// Entries may wrap the fields in an embedded "mix_info".
func MixInfoOf(entry map[string]any) MixInfo {
	info := entry
	if mi := mapGet(entry, "mix_info"); mi != nil {
		info = mi
	}
	out := MixInfo{
		ID:    mapStr(info, "mix_id"),
		Name:  mapStr(info, "mix_name"),
		Count: int(mapFloat(info, "total_item_count")),
	}
	if out.ID == "" {
		out.ID = mapStr(info, "id")
	}
	if out.Name == "" {
		out.Name = mapStr(info, "title")
	}
	if urls := urlList(mapGet(info, "cover_url")); len(urls) > 0 {
		out.Cover = urls[0]
	} else {
		out.Cover = mapStr(info, "cover_url")
	}
	return out
}

// ProgressFunc receives (downloadedBytes, totalBytes); total may be -1.
type ProgressFunc func(done, total int64)

// DownloadFile streams a media URL to dstPath (via a .part file, renamed on
// success). Retries are handled by the caller across candidates.
func (c *Client) DownloadFile(ctx context.Context, cand Candidate, dstPath string, progress ProgressFunc) error {
	req, err := httpNewRequest(ctx, "GET", cand.URL)
	if err != nil {
		return err
	}
	for k, vs := range cand.Headers {
		req.Header[k] = vs
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 206 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/html") {
		return fmt.Errorf("got HTML instead of media (WAF page?)")
	}
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return err
	}
	tmp := dstPath + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	pw := &progressWriter{w: f, total: resp.ContentLength, fn: progress}
	_, copyErr := io.CopyBuffer(pw, resp.Body, make([]byte, 256*1024))
	closeErr := f.Close()
	if copyErr != nil {
		os.Remove(tmp)
		return copyErr
	}
	if closeErr != nil {
		os.Remove(tmp)
		return closeErr
	}
	return os.Rename(tmp, dstPath)
}

// progressWriter reports cumulative bytes written.
type progressWriter struct {
	w     io.Writer
	done  int64
	total int64
	fn    ProgressFunc
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.done += int64(n)
	if p.fn != nil {
		p.fn(p.done, p.total)
	}
	return n, err
}
