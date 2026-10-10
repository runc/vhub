package douyin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	baseURL = "https://www.douyin.com"

	// argusMarker identifies Douyin's deterministic anti-bot rejection:
	// HTTP 403 whose body reads "Blocked by ArgusSecurityPlugin …". Retrying
	// or re-signing cannot help — the request shape itself is refused.
	argusMarker = "ArgusSecurityPlugin"
)

var userAgentPool = []string{
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/139.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/139.0.0.0 Safari/537.36",
}

// Risk-control error categories surfaced to the UI.
var (
	ErrArgusRejected = errors.New("douyin risk control rejected the request (ArgusSecurityPlugin)")
	ErrLoginRequired = errors.New("douyin requires a logged-in session (check the cookie)")
)

// Bridge fetches a URL inside a douyin.com page context, where the site's
// own anti-bot SDK signs the request. Implemented by the app's hidden
// webview bridge; nil keeps the plain-HTTP path.
type Bridge interface {
	Fetch(ctx context.Context, rawURL string) (status int, body []byte, err error)
}

// Options configure a Client.
type Options struct {
	Cookie    string // raw "a=b; c=d" header value from the browser
	Proxy     string // http(s):// or socks5:// URL, "" for direct
	UserAgent string // "" picks one at random
	Bridge    Bridge // optional page-context signer for Argus-gated endpoints
}

// Client talks to Douyin's web API. API calls are signed with X-Bogus and
// throttled with a jittered rate limiter, mirroring the reference
// implementation's anti-bot posture.
type Client struct {
	hc        *http.Client
	noRedir   *http.Client
	userAgent string
	cookie    string
	signer    *xBogusSigner
	tokens    *msTokenSource
	limiter   *rateLimiter
	bridge    Bridge
}

func NewClient(opts Options) *Client {
	ua := opts.UserAgent
	if ua == "" {
		ua = userAgentPool[rand.Intn(len(userAgentPool))]
	}
	transport := &http.Transport{
		MaxIdleConns:        16,
		IdleConnTimeout:     60 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		Proxy:               http.ProxyFromEnvironment,
	}
	if opts.Proxy != "" {
		if pu, err := url.Parse(opts.Proxy); err == nil {
			transport.Proxy = http.ProxyURL(pu)
		}
	}
	hc := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	noRedir := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &Client{
		hc:        hc,
		noRedir:   noRedir,
		userAgent: ua,
		cookie:    strings.TrimSpace(opts.Cookie),
		signer:    newXBogusSigner(ua),
		tokens:    newMsTokenSource(ua, hc),
		limiter:   newRateLimiter(2),
		bridge:    opts.Bridge,
	}
}

func (c *Client) noRedirectClient() *http.Client { return c.noRedir }

// SetRateLimit adjusts the request throttle (requests/second, >0).
func (c *Client) SetRateLimit(rps float64) { c.limiter.set(rps) }

// Cookie reports whether a session cookie is configured.
func (c *Client) HasCookie() bool { return c.cookie != "" }

func httpNewRequest(ctx context.Context, method, rawurl string) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, method, rawurl, nil)
}

// readLimitedBody reads a response body with a hard size cap.
func readLimitedBody(resp *http.Response, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// defaultQuery mirrors _default_query of the reference implementation: the
// full browser-parameter set a real Chrome client sends.
func (c *Client) defaultQuery(ctx context.Context) url.Values {
	v := url.Values{
		"device_platform":      {"webapp"},
		"aid":                  {"6383"},
		"channel":              {"channel_pc_web"},
		"update_version_code":  {"170400"},
		"pc_client_type":       {"1"},
		"pc_libra_divert":      {"Windows"},
		"version_code":         {"290100"},
		"version_name":         {"29.1.0"},
		"cookie_enabled":       {"true"},
		"screen_width":         {"1536"},
		"screen_height":        {"864"},
		"browser_language":     {"zh-CN"},
		"browser_platform":     {"Win32"},
		"browser_name":         {"Chrome"},
		"browser_version":      {"139.0.0.0"},
		"browser_online":       {"true"},
		"engine_name":          {"Blink"},
		"engine_version":       {"139.0.0.0"},
		"os_name":              {"Windows"},
		"os_version":           {"10"},
		"cpu_core_num":         {"16"},
		"device_memory":        {"8"},
		"platform":             {"PC"},
		"downlink":             {"10"},
		"effective_type":       {"4g"},
		"round_trip_time":      {"200"},
		"support_h265":         {"1"},
		"support_dash":         {"1"},
		"uifid":                {""},
		"msToken":              {c.tokens.Get(ctx)},
	}
	return v
}

// SignURL appends an X-Bogus signature to an arbitrary URL and returns the
// signed URL plus the user agent the signature was computed for.
func (c *Client) SignURL(rawurl string) (string, string) {
	signed, _, ua := c.signer.build(rawurl)
	return signed, ua
}

var retryDelays = []time.Duration{time.Second, 2 * time.Second, 5 * time.Second}

const maxAttempts = 3

// getJSON performs a signed GET and decodes the JSON body. Retries on
// transient risk-control signals (403/429, empty 200, 5xx, network errors);
// fails fast on Argus rejections and login-required responses.
//
// When a Bridge is configured the request goes through the douyin page
// first: the site SDK's signature is the only shape Argus accepts since
// 2026-09, so gated endpoints (aweme/post, mix/*, music/*, detail) only
// answer there. Deterministic bridge failures are returned as-is;
// infrastructure problems (window gone, eval error, exhaustion of
// transient retries) fall through to the direct path as a last resort.
func (c *Client) getJSON(ctx context.Context, path string, params url.Values) (map[string]any, error) {
	if c.bridge != nil {
		data, err := c.getJSONViaBridge(ctx, path, params)
		if err == nil {
			return data, nil
		}
		if errors.Is(err, ErrArgusRejected) || errors.Is(err, ErrLoginRequired) {
			return nil, err
		}
	}
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := c.limiter.wait(ctx); err != nil {
			return nil, err
		}
		signedURL, ua := c.SignURL(baseURL + path + "?" + params.Encode())
		req, err := httpNewRequest(ctx, "GET", signedURL)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", ua)
		req.Header.Set("Referer", baseURL+"/?recommend=1")
		req.Header.Set("Accept", "*/*")
		req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en-US;q=0.8,en;q=0.7")
		if c.cookie != "" {
			req.Header.Set("Cookie", c.cookie)
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			lastErr = err
		} else {
			body, readErr := readLimitedBody(resp, 16<<20)
			resp.Body.Close()
			if readErr != nil {
				lastErr = readErr
			} else {
				retry, done, out, ferr := c.handleResponse(path, resp.StatusCode, body)
				if ferr != nil {
					return nil, ferr
				}
				if done {
					return out, nil
				}
				if !retry {
					return nil, fmt.Errorf("douyin API %s: HTTP %d", path, resp.StatusCode)
				}
				lastErr = fmt.Errorf("douyin API %s: HTTP %d (transient)", path, resp.StatusCode)
			}
		}
		if attempt < maxAttempts-1 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(retryDelays[min(attempt, len(retryDelays)-1)]):
			}
		}
	}
	return nil, fmt.Errorf("douyin API %s failed after %d attempts: %w", path, maxAttempts, lastErr)
}

// getJSONViaBridge issues the unsigned URL inside the douyin page — the
// site SDK adds uifid/timestamp/signature itself — and classifies the
// response with the same rules as the direct path.
func (c *Client) getJSONViaBridge(ctx context.Context, path string, params url.Values) (map[string]any, error) {
	// Drop uifid: an explicitly empty value stops the page SDK from
	// injecting its signed one, and Argus then rejects the request.
	p := make(url.Values, len(params))
	for k, vs := range params {
		if k == "uifid" {
			continue
		}
		p[k] = vs
	}
	raw := baseURL + path + "?" + p.Encode()
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := c.limiter.wait(ctx); err != nil {
			return nil, err
		}
		status, body, err := c.bridge.Fetch(ctx, raw)
		if err != nil {
			return nil, err // window/eval failure: caller falls back to direct
		}
		retry, done, out, ferr := c.handleResponse(path, status, body)
		if ferr != nil {
			return nil, ferr
		}
		if done {
			return out, nil
		}
		if !retry {
			return nil, fmt.Errorf("douyin API %s via page: HTTP %d", path, status)
		}
		lastErr = fmt.Errorf("douyin API %s via page: HTTP %d (transient)", path, status)
		if attempt < maxAttempts-1 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(retryDelays[min(attempt, len(retryDelays)-1)]):
			}
		}
	}
	return nil, fmt.Errorf("douyin API %s via page failed after %d attempts: %w", path, maxAttempts, lastErr)
}

// handleResponse classifies one API response.
//   - ferr: deterministic failure — return it now (Argus / login required)
//   - done: success — out is the decoded JSON
//   - retry: transient — retry with a fresh signature
func (c *Client) handleResponse(path string, status int, body []byte) (retry, done bool, out map[string]any, ferr error) {
	if status == 200 {
		if len(body) == 0 {
			// Empty 200 is a common anti-bot signal from Douyin.
			return true, false, nil, nil
		}
		var data map[string]any
		if err := json.Unmarshal(body, &data); err != nil {
			// Non-JSON 200: captcha page or challenge HTML.
			return true, false, nil, nil
		}
		if isLoginRequired(data) {
			return false, false, nil, ErrLoginRequired
		}
		return false, true, data, nil
	}
	prefix := string(body[:min(len(body), 512)])
	if status == 403 && strings.Contains(prefix, argusMarker) {
		return false, false, nil, fmt.Errorf("%w: %.80s", ErrArgusRejected, prefix)
	}
	if status == 403 || status == 429 || status >= 500 {
		return true, false, nil, nil
	}
	return false, false, nil, nil // other 4xx: terminal, not retried
}

func isLoginRequired(data map[string]any) bool {
	code, _ := data["status_code"].(float64)
	msg, _ := data["status_msg"].(string)
	return int(code) == 2483 || strings.Contains(msg, "请先登录") || strings.Contains(msg, "用户未登录")
}

// apiCall is the shared endpoint helper: default query + caller params.
func (c *Client) apiCall(ctx context.Context, path string, extra url.Values) (map[string]any, error) {
	params := c.defaultQuery(ctx)
	for k, vs := range extra {
		params[k] = vs
	}
	return c.getJSON(ctx, path, params)
}

// GetVideoDetail returns aweme_detail for one video/note id. Like the
// reference implementation it tries aid=6383 (covers notes) then aid=1128
// (covers videos filtered from the other aid's response).
func (c *Client) GetVideoDetail(ctx context.Context, awemeID string) (map[string]any, error) {
	for _, aid := range []string{"6383", "1128"} {
		data, err := c.apiCall(ctx, "/aweme/v1/web/aweme/detail/", url.Values{
			"aweme_id": {awemeID},
			"aid":      {aid},
		})
		if err != nil {
			return nil, err
		}
		if detail, ok := data["aweme_detail"].(map[string]any); ok && len(detail) > 0 {
			return detail, nil
		}
		if fd, ok := data["filter_detail"].(map[string]any); ok && fd["filter_reason"] != nil {
			continue // filtered for this aid — try the other
		}
		break // aweme_detail null without a filter reason: give up
	}
	return nil, fmt.Errorf("no aweme_detail for %s", awemeID)
}

// pagedResp is one normalized page of a list endpoint.
type pagedResp struct {
	Items    []map[string]any
	HasMore  bool
	MaxCursor int64
	Raw      map[string]any
}

func normalizePaged(raw map[string]any, itemKeys ...string) pagedResp {
	p := pagedResp{Raw: raw}
	for _, key := range append(itemKeys, "aweme_list", "mix_list", "music_list") {
		v, ok := raw[key]
		if !ok {
			continue
		}
		if list, ok := v.([]any); ok {
			for _, it := range list {
				if m, ok := it.(map[string]any); ok {
					p.Items = append(p.Items, m)
				}
			}
			break
		}
		break // key present but null/non-list: keep empty items
	}
	switch hm := raw["has_more"].(type) {
	case float64:
		p.HasMore = hm != 0
	case bool:
		p.HasMore = hm
	}
	cur := raw["max_cursor"]
	if cur == nil {
		cur = raw["cursor"]
	}
	if f, ok := cur.(float64); ok {
		p.MaxCursor = int64(f)
	}
	return p
}

// GetUserPost fetches one page of a profile's posts.
func (c *Client) GetUserPost(ctx context.Context, secUID string, maxCursor int64) (pagedResp, error) {
	raw, err := c.apiCall(ctx, "/aweme/v1/web/aweme/post/", url.Values{
		"sec_user_id":                  {secUID},
		"max_cursor":                   {fmt.Sprint(maxCursor)},
		"count":                        {"18"},
		"locate_query":                 {"false"},
		"show_live_replay_strategy":    {"1"},
		"need_time_list":               {"1"},
		"time_list_query":              {"0"},
		"whale_cut_token":              {""},
		"cut_version":                  {"1"},
		"publish_video_strategy_type":  {"2"},
	})
	if err != nil {
		return pagedResp{}, err
	}
	return normalizePaged(raw, "aweme_list"), nil
}

// GetUserMix fetches one page of a profile's mix (collection) list.
func (c *Client) GetUserMix(ctx context.Context, secUID string, maxCursor int64) (pagedResp, error) {
	raw, err := c.apiCall(ctx, "/aweme/v1/web/mix/list/", url.Values{
		"sec_user_id": {secUID},
		"max_cursor":  {fmt.Sprint(maxCursor)},
		"count":       {"20"},
		"locate_query": {"false"},
	})
	if err != nil {
		return pagedResp{}, err
	}
	return normalizePaged(raw, "mix_infos", "mix_list"), nil
}

// GetMixAweme fetches one page of a mix's items.
func (c *Client) GetMixAweme(ctx context.Context, mixID string, maxCursor int64) (pagedResp, error) {
	raw, err := c.apiCall(ctx, "/aweme/v1/web/mix/aweme/", url.Values{
		"mix_id":     {mixID},
		"max_cursor": {fmt.Sprint(maxCursor)},
		"count":      {"30"},
	})
	if err != nil {
		return pagedResp{}, err
	}
	return normalizePaged(raw, "aweme_list"), nil
}

// GetMusicAweme fetches one page of a music's items.
func (c *Client) GetMusicAweme(ctx context.Context, musicID string, maxCursor int64) (pagedResp, error) {
	raw, err := c.apiCall(ctx, "/aweme/v1/web/music/aweme/", url.Values{
		"music_id":   {musicID},
		"max_cursor": {fmt.Sprint(maxCursor)},
		"count":      {"30"},
	})
	if err != nil {
		return pagedResp{}, err
	}
	return normalizePaged(raw, "aweme_list"), nil
}

// DownloadHeaders returns the headers media (CDN) requests need.
func (c *Client) DownloadHeaders(ua string) http.Header {
	if ua == "" {
		ua = c.userAgent
	}
	h := http.Header{
		"Referer":    {baseURL + "/"},
		"Origin":     {baseURL},
		"Accept":     {"*/*"},
		"User-Agent": {ua},
	}
	return h
}

// HTTP returns the shared HTTP client (media downloads, short-URL resolve).
func (c *Client) HTTP() *http.Client { return c.hc }

// UserAgent returns the UA this client instance signs requests for.
func (c *Client) UserAgent() string { return c.userAgent }

// rateLimiter enforces a minimum interval between requests plus a random
// jitter, like control/rate_limiter.py.
type rateLimiter struct {
	mu       sync.Mutex
	minDelta time.Duration
	last     time.Time
}

func newRateLimiter(rps float64) *rateLimiter {
	l := &rateLimiter{}
	l.set(rps)
	return l
}

func (l *rateLimiter) set(rps float64) {
	if rps <= 0 {
		rps = 2
	}
	l.mu.Lock()
	l.minDelta = time.Duration(float64(time.Second) / rps)
	l.mu.Unlock()
}

func (l *rateLimiter) wait(ctx context.Context) error {
	l.mu.Lock()
	now := time.Now()
	wait := l.minDelta - now.Sub(l.last)
	if wait < 0 {
		wait = 0
	}
	wait += time.Duration(rand.Float64() * 0.5 * float64(time.Second))
	l.last = now.Add(wait)
	l.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(wait):
		return nil
	}
}
