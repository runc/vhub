package douyin

import (
	"strings"
	"testing"
)

func TestExtractURLs(t *testing.T) {
	text := `7.94 复制打开抖音，看看【阿呆的作品】可能认识的人  https://v.douyin.com/iABC123x/  复制此链接，打开Dou音搜索
https://www.douyin.com/video/7123456789012345678 and https://example.com/not-douyin
https://v.douyin.com/iABC123x/ again (dup)`
	got := ExtractURLs(text)
	if len(got) != 2 {
		t.Fatalf("want 2 deduped douyin urls, got %v", got)
	}
	if got[0] != "https://v.douyin.com/iABC123x/" || got[1] != "https://www.douyin.com/video/7123456789012345678" {
		t.Fatalf("unexpected order/content: %v", got)
	}
}

func TestExtractURLsNone(t *testing.T) {
	if got := ExtractURLs("no links at all https://google.com/x"); len(got) != 0 {
		t.Fatalf("want none, got %v", got)
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		url     string
		typ     LinkType
		idField string
	}{
		{"https://www.douyin.com/video/7234567890123456789", TypeVideo, "7234567890123456789"},
		{"https://www.douyin.com/note/7234567890123456788", TypeGallery, "7234567890123456788"},
		{"https://www.douyin.com/collection/7299888877776666", TypeMix, "7299888877776666"},
		{"https://www.douyin.com/music/6812345678901234567", TypeMusic, "6812345678901234567"},
		{"https://www.douyin.com/discover?modal_id=7234567890123456789", TypeVideo, "7234567890123456789"},
	}
	for _, c := range cases {
		l := Classify(c.url)
		if l == nil {
			t.Fatalf("%s: not classified", c.url)
		}
		if l.Type != c.typ || l.ID != c.idField {
			t.Fatalf("%s: got %s/%s want %s/%s", c.url, l.Type, l.ID, c.typ, c.idField)
		}
	}
}

func TestClassifyUser(t *testing.T) {
	l := Classify("https://www.douyin.com/user/MS4wLjABAAAAabcd1234")
	if l == nil || l.Type != TypeUser || l.SecUID != "MS4wLjABAAAAabcd1234" {
		t.Fatalf("user link misparsed: %+v", l)
	}
}

func TestClassifyRejects(t *testing.T) {
	for _, u := range []string{
		"https://www.douyin.com/",
		"https://www.douyin.com/search/foo",
		"https://example.com/video/123",
	} {
		if l := Classify(u); l != nil {
			t.Fatalf("%s: want nil, got %+v", u, l)
		}
	}
}

func TestXBogusFormat(t *testing.T) {
	s := newXBogusSigner("")
	signed, xb, ua := s.build("https://www.douyin.com/aweme/v1/web/aweme/detail/?aweme_id=123&aid=6383")
	if !strings.HasSuffix(signed, "&X-Bogus="+xb) {
		t.Fatalf("signed url missing suffix: %s", signed)
	}
	if len(xb) != 28 {
		t.Fatalf("X-Bogus length: want 28, got %d (%s)", len(xb), xb)
	}
	for _, r := range xb {
		if !strings.ContainsRune(xBogusCharacter, r) {
			t.Fatalf("X-Bogus has out-of-alphabet char %q", r)
		}
	}
	if ua == "" || !strings.Contains(ua, "Chrome") {
		t.Fatalf("unexpected UA %q", ua)
	}
}

func TestXBogusUAConsistency(t *testing.T) {
	s := newXBogusSigner("test-agent")
	_, _, ua := s.build("https://www.douyin.com/x?a=1")
	if ua != "test-agent" {
		t.Fatalf("UA not propagated: %q", ua)
	}
}
