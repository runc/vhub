package douyin

import (
	"context"
	"regexp"
	"strings"
)

// LinkType classifies a Douyin share link (ported from core/url_parser.py).
type LinkType string

const (
	TypeVideo   LinkType = "video"
	TypeGallery LinkType = "gallery"
	TypeUser    LinkType = "user"
	TypeMix     LinkType = "collection"
	TypeMusic   LinkType = "music"
)

// Link is a parsed share link.
type Link struct {
	Type    LinkType
	URL     string // resolved URL (short links unfolded)
	ID      string // aweme_id / mix_id / music_id
	SecUID  string // user links
	Display string // short label for the UI
}

var urlRe = regexp.MustCompile(`https?://[^\s"'<>，。、；！）)\]】]+`)

// ExtractURLs pulls Douyin URLs out of pasted share text ("复制此链接，打开
// Dou音搜索…" style text included), de-duplicated, in order of appearance.
func ExtractURLs(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range urlRe.FindAllString(text, -1) {
		u := strings.TrimRight(m, ".,;:!?)>\"'")
		if !isDouyinURL(u) || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	return out
}

func isDouyinURL(u string) bool {
	return strings.Contains(u, "douyin.com") || strings.Contains(u, "iesdouyin.com")
}

func isShortURL(u string) bool {
	return strings.Contains(u, "v.douyin.com") || strings.Contains(u, "iesdouyin.com")
}

var secUIDRe = regexp.MustCompile(`^MS4wLjAB[A-Za-z0-9_-]+$`)

// SecUIDFromText returns a bare sec_uid pasted on its own (no URL), else "".
func SecUIDFromText(text string) string {
	t := strings.TrimSpace(text)
	if secUIDRe.MatchString(t) {
		return t
	}
	return ""
}

var (
	videoRe = regexp.MustCompile(`/(?:video|note|gallery|slides)/(\d+)`)
	modalRe = regexp.MustCompile(`modal_id=(\d+)`)
	userRe  = regexp.MustCompile(`/user/([A-Za-z0-9_-]+)`)
	mixRe   = regexp.MustCompile(`/(?:collection|mix)/(\d+)`)
	musicRe = regexp.MustCompile(`/music/(\d+)`)
	noteRe  = regexp.MustCompile(`/(?:note|gallery|slides)/(\d+)`)
)

// Classify identifies the link kind and extracts the target id. The URL must
// already be resolved (see Resolve).
func Classify(u string) *Link {
	if !isDouyinURL(u) {
		return nil
	}
	if m := noteRe.FindStringSubmatch(u); m != nil {
		return &Link{Type: TypeGallery, URL: u, ID: m[1], Display: "图集 " + m[1]}
	}
	if m := videoRe.FindStringSubmatch(u); m != nil {
		return &Link{Type: TypeVideo, URL: u, ID: m[1], Display: "视频 " + m[1]}
	}
	if m := userRe.FindStringSubmatch(u); m != nil {
		return &Link{Type: TypeUser, URL: u, SecUID: m[1], Display: "主页 " + m[1]}
	}
	if m := mixRe.FindStringSubmatch(u); m != nil {
		return &Link{Type: TypeMix, URL: u, ID: m[1], Display: "合集 " + m[1]}
	}
	if m := musicRe.FindStringSubmatch(u); m != nil {
		return &Link{Type: TypeMusic, URL: u, ID: m[1], Display: "音乐 " + m[1]}
	}
	if m := modalRe.FindStringSubmatch(u); m != nil {
		return &Link{Type: TypeVideo, URL: u, ID: m[1], Display: "视频 " + m[1]}
	}
	return nil
}

// Resolve unfolds v.douyin.com / iesdouyin.com short links via the redirect
// Location header; other URLs are returned unchanged.
func (c *Client) Resolve(ctx context.Context, u string) (string, error) {
	if !isShortURL(u) {
		return u, nil
	}
	req, err := httpNewRequest(ctx, "GET", u)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.noRedirectClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "" {
		return loc, nil
	}
	return u, nil
}
