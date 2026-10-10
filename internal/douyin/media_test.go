package douyin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func awemeVideo(urls ...string) map[string]any {
	return map[string]any{
		"aweme_id": "12345",
		"desc":     "test video",
		"video": map[string]any{
			"play_addr": map[string]any{"url_list": toAny(urls)},
		},
	}
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func TestVideoCandidatesPrefersNoWatermarkCDN(t *testing.T) {
	c := NewClient(Options{})
	aw := awemeVideo(
		"https://www.douyin.com/aweme/v1/play/?video_id=v1&watermark=1",
		"https://v26-cdn.douyinvod.com/media.mp4?watermark=0",
		"https://v3-cdn.douyinvod.com/media.mp4?watermark=0",
	)
	cands := c.VideoCandidates(aw, QualityHighest)
	if len(cands) == 0 {
		t.Fatal("no candidates")
	}
	if !strings.Contains(cands[0].URL, "douyinvod.com") {
		t.Fatalf("first candidate should be CDN direct, got %s", cands[0].URL)
	}
	// The watermarked douyin.com play endpoint is demoted to the last resort
	// and re-signed.
	last := cands[len(cands)-1].URL
	if !strings.Contains(last, "douyin.com/aweme/v1/play/") || !strings.Contains(last, "X-Bogus=") {
		t.Fatalf("watermarked play endpoint should be the signed fallback, got %v", last)
	}
}

func TestVideoCandidatesConstructsPlayURL(t *testing.T) {
	c := NewClient(Options{})
	aw := map[string]any{
		"aweme_id": "12345",
		"video": map[string]any{
			"play_addr": map[string]any{"uri": "v0200f0000abc", "url_list": toAny([]string{})},
		},
	}
	cands := c.VideoCandidates(aw, QualityHighest)
	if len(cands) != 1 {
		t.Fatalf("want 1 constructed candidate, got %d", len(cands))
	}
	if !strings.Contains(cands[0].URL, "video_id=v0200f0000abc") || !strings.Contains(cands[0].URL, "X-Bogus=") {
		t.Fatalf("constructed play URL malformed: %s", cands[0].URL)
	}
}

func TestGalleryDetection(t *testing.T) {
	aw := map[string]any{
		"aweme_id":   "99",
		"aweme_type": float64(68),
		"images": toAnyMaps([]map[string]any{
			{"display_image": map[string]any{"url_list": toAny([]string{"https://img1"})}},
			{"origin_image": map[string]any{"url_list": toAny([]string{"https://img2a", "https://img2b"})}},
		}),
	}
	if DetectMediaKind(aw) != KindGallery {
		t.Fatal("want gallery")
	}
	sets := GalleryImageURLs(aw)
	if len(sets) != 2 || sets[1][0] != "https://img2a" {
		t.Fatalf("unexpected image sets: %v", sets)
	}
}

func toAnyMaps(ms []map[string]any) []any {
	out := make([]any, len(ms))
	for i, m := range ms {
		out[i] = m
	}
	return out
}

func TestDownloadFile(t *testing.T) {
	payload := []byte(strings.Repeat("x", 1024))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/html" {
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html>captcha</html>"))
			return
		}
		w.Write(payload)
	}))
	defer srv.Close()

	c := NewClient(Options{})
	dst := filepath.Join(t.TempDir(), "out.mp4")
	var done, total int64
	err := c.DownloadFile(context.Background(), Candidate{URL: srv.URL + "/v"}, dst, func(d, t int64) { done, total = d, t })
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != string(payload) {
		t.Fatal("payload mismatch")
	}
	if done != 1024 || total != 1024 {
		t.Fatalf("progress: %d/%d", done, total)
	}
	if _, err := os.Stat(dst + ".part"); !os.IsNotExist(err) {
		t.Fatal(".part file left behind")
	}

	err = c.DownloadFile(context.Background(), Candidate{URL: srv.URL + "/html"}, dst, nil)
	if err == nil || !strings.Contains(err.Error(), "HTML") {
		t.Fatalf("want HTML-detection error, got %v", err)
	}
}

func TestHandleResponseClassification(t *testing.T) {
	c := NewClient(Options{})
	// Argus 403 → deterministic
	if retry, _, _, ferr := c.handleResponse("/x", 403, []byte(`{"error":"Blocked by ArgusSecurityPlugin"}`)); retry || ferr == nil {
		t.Fatal("Argus 403 must be terminal")
	}
	// plain 403 → transient
	if retry, _, _, _ := c.handleResponse("/x", 403, []byte("forbidden")); !retry {
		t.Fatal("plain 403 must retry")
	}
	// empty 200 → transient
	if retry, _, _, _ := c.handleResponse("/x", 200, nil); !retry {
		t.Fatal("empty 200 must retry")
	}
	// login required → terminal
	if _, _, _, ferr := c.handleResponse("/x", 200, []byte(`{"status_code":2483}`)); ferr == nil {
		t.Fatal("login-required must be terminal")
	}
	// normal JSON → done
	if _, done, out, _ := c.handleResponse("/x", 200, []byte(`{"a":1}`)); !done || out["a"] != float64(1) {
		t.Fatal("normal JSON must decode")
	}
}
