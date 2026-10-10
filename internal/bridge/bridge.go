// Package bridge routes Douyin API calls through a webview sitting on
// douyin.com. The site's own Argus SDK signs every fetch issued from the
// page (uifid / timestamp / x-secsdk-web-signature) — a signature the
// plain-HTTP path cannot reproduce, so gated endpoints hard-403 without it.
// The webview follows the system proxy and keeps its own persistent cookie
// store; logging in once inside the bridge window authenticates later
// fetches across restarts.
package bridge

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/egoist/mygo"
)

type PageBridge struct {
	mu      sync.Mutex
	win     *mygo.Window
	page    *mygo.Page
	readyAt time.Time
}

const (
	bridgeHome = "https://www.douyin.com/"
	// Grace period after the page reports loaded, for its signing SDK to
	// boot before the first fetch goes out.
	bridgeReadyGrace = 5 * time.Second
	bridgeReadyWait  = 45 * time.Second
)

// ensure returns a live page, creating the window on first use and after
// the user closes it. Called on worker goroutines; mygo marshals to the
// main thread internally.
func (b *PageBridge) ensure() (*mygo.Page, time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.win != nil && b.win.IsDestroyed() {
		b.win, b.page = nil, nil
	}
	if b.win == nil {
		b.open(false)
	}
	return b.page, b.readyAt
}

// open creates the bridge window; caller holds b.mu.
func (b *PageBridge) open(visible bool) {
	w := mygo.NewWindow(mygo.WindowOptions{
		Title:  "VHub · 抖音",
		URL:    bridgeHome,
		Hidden: !visible,
		Width:  1180,
		Height: 780,
	})
	b.win, b.page = w, w.Page()
	b.readyAt = time.Now().Add(bridgeReadyGrace)
}

// waitReady blocks until the douyin page has loaded and the grace period
// for its signing SDK has passed.
func waitReady(ctx context.Context, p *mygo.Page, readyAt time.Time) error {
	deadline := time.Now().Add(bridgeReadyWait)
	for {
		if v, err := p.EvalContext(ctx, "return document.readyState"); err == nil {
			if s, _ := v.(string); s == "complete" {
				break
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("douyin 页面加载超时")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	if d := time.Until(readyAt); d > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}
	}
	return nil
}

// Fetch issues a GET on rawURL inside the douyin page. The response status
// and raw body are returned; a nil error means the request reached the
// server (status may still be a risk-control 4xx for the caller to judge).
func (b *PageBridge) Fetch(ctx context.Context, rawURL string) (int, []byte, error) {
	p, readyAt := b.ensure()
	if p == nil {
		return 0, nil, fmt.Errorf("bridge window unavailable")
	}
	if err := waitReady(ctx, p, readyAt); err != nil {
		return 0, nil, err
	}
	code := fmt.Sprintf(`return await fetch(%q, {credentials:"include", headers:{"Accept":"*/*"}})
		.then(async r => ({status:r.status, text:await r.text()}))
		.catch(e => ({status:0, text:String(e)}))`, rawURL)
	v, err := p.EvalContext(ctx, code)
	if err != nil {
		return 0, nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return 0, nil, fmt.Errorf("bridge: unexpected eval result")
	}
	status, _ := m["status"].(float64)
	text, _ := m["text"].(string)
	if int(status) == 0 {
		return 0, nil, fmt.Errorf("bridge: in-page fetch failed: %s", text)
	}
	return int(status), []byte(text), nil
}

// ShowLogin makes the bridge window visible so the user can log in by QR
// code. Closing the window hides the bridge again; the next Fetch creates
// a fresh hidden window on the same cookie store.
func (b *PageBridge) ShowLogin() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.win == nil || b.win.IsDestroyed() {
		b.open(true)
		return
	}
	b.win.Show()
	b.win.Focus()
}
