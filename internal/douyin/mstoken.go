package douyin

import (
	"bytes"
	"context"
	"encoding/json"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"
)

// msToken mirrors auth/ms_token_manager.py: prefer a real msToken minted by
// the mssdk endpoint (config snapshot bundled from the f2 project), fall back
// to a random 184-char token when the endpoint is unreachable.
//
// Douyin answers 403 to /aweme/post/ after a few pages with a random token,
// so the real-token path matters for profile walks.

var bundledMsTokenConf = struct {
	URL      string
	Magic    int
	Version  int
	DataType int
	Ulr      int
	StrData  string
}{
	URL: "https://mssdk.bytedance.com/web/r/token?ms_appid=6383&msToken=T4bNG9W2rKF7hBNwaYssDE" +
		"rnJEobDAk641DFaOn4hcsfAM8slpbZeKPM4Ml4rhDQq18iY8nQ0JR3J87SLZtDiDqtZdZawfBjCWAgtolQso" +
		"EtG6MLETvo4fwr7F28zGJUFDdJgKEZHibNR0QshVBv28ygsQsJDzerKAtsgj9Pn5WsxyS1vfkiX3I%3D",
	Magic:    538969122,
	Version:  1,
	DataType: 8,
	Ulr:      0,
	StrData: msTokenStrData,
}

const msTokenChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

func randomMsToken() string {
	var b strings.Builder
	b.Grow(184)
	for i := 0; i < 182; i++ {
		b.WriteByte(msTokenChars[rand.Intn(len(msTokenChars))])
	}
	b.WriteString("==")
	return b.String()
}

func validMsToken(t string) bool {
	n := len(strings.TrimSpace(t))
	return n == 164 || n == 184
}

// msTokenSource mints and caches msTokens with the same budgets as the
// Python manager: 60s token TTL, 300s cooldown after a failed generation.
type msTokenSource struct {
	mu         sync.Mutex
	cached     string
	cachedAt   time.Time
	retryAfter time.Time
	userAgent  string
	httpClient *http.Client
}

func newMsTokenSource(ua string, hc *http.Client) *msTokenSource {
	return &msTokenSource{userAgent: ua, httpClient: hc}
}

// Get returns a usable msToken: a cookie-provided one first, then a cached
// real token, then a fresh real token, finally the random fallback.
func (s *msTokenSource) Get(ctx context.Context) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if s.cached != "" && now.Sub(s.cachedAt) < 60*time.Second {
		return s.cached
	}
	if now.Before(s.retryAfter) {
		return randomMsToken()
	}
	if t := s.generate(ctx); t != "" {
		s.cached = t
		s.cachedAt = now
		s.retryAfter = time.Time{}
		return t
	}
	s.retryAfter = now.Add(300 * time.Second)
	return randomMsToken()
}

func (s *msTokenSource) generate(ctx context.Context) string {
	conf := bundledMsTokenConf
	payload, _ := json.Marshal(map[string]any{
		"magic":         conf.Magic,
		"version":       conf.Version,
		"dataType":      conf.DataType,
		"strData":       conf.StrData,
		"ulr":           conf.Ulr,
		"tspFromClient": time.Now().UnixMilli(),
	})
	req, err := http.NewRequestWithContext(ctx, "POST", conf.URL, bytes.NewReader(payload))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("User-Agent", s.userAgent)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Name == "msToken" && validMsToken(c.Value) {
			return strings.TrimSpace(c.Value)
		}
	}
	return ""
}
