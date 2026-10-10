package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/egoist/mygo"
	"vhub/internal/bridge"
	"vhub/internal/douyin"
	"vhub/internal/queue"
)

// JobsChanged is broadcast with the full job table whenever it changes.
var JobsChanged = mygo.NewEvent[[]*queue.Job]("vhub:jobs")

// UsersChanged is broadcast with the follow list whenever it mutates.
var UsersChanged = mygo.NewEvent[[]*FollowedUser]("vhub:users")

// SubmitResult reports what a paste produced.
type SubmitResult struct {
	Created []string `json:"created"` // job ids
	Skipped []string `json:"skipped"` // urls that could not be classified
	Failed  []string `json:"failed"`  // urls that errored resolving
}

// Hub is the bound service the UI calls into.
type Hub struct {
	mu       sync.Mutex
	settings *Settings
	client   atomic.Pointer[douyin.Client]
	manager  *queue.Manager
	bridge   *bridge.PageBridge
	users    *Users
}

// NewHub wires the download engine to the bound service.
func NewHub(settings *Settings) *Hub {
	users, _ := LoadUsers() // unreadable file degrades to an empty list
	if users == nil {
		users = &Users{awemes: map[string]map[string]map[string]any{}}
	}
	h := &Hub{settings: settings, bridge: &bridge.PageBridge{}, users: users}
	h.rebuildClient(settings.Get())
	h.manager = queue.NewManager(queue.Options{
		Client:      h.client.Load(),
		DownloadDir: settings.Get().DownloadDir,
		Quality:     douyin.Quality(settings.Get().Quality),
		Workers:     settings.Get().Workers,
		OnChange: func(jobs []*queue.Job) {
			_ = JobsChanged.Broadcast(jobs)
		},
	})
	settings.onUpdate = func(data SettingsData) {
		h.rebuildClient(data)
		h.manager.Update(queue.Options{
			Client:      h.client.Load(),
			DownloadDir: data.DownloadDir,
			Quality:     douyin.Quality(data.Quality),
			Workers:     data.Workers,
		})
	}
	return h
}

func (h *Hub) rebuildClient(data SettingsData) {
	h.client.Store(douyin.NewClient(douyin.Options{
		Cookie: data.Cookie,
		Proxy:  data.Proxy,
		Bridge: h.bridge,
	}))
	h.client.Load().SetRateLimit(data.RateLimit)
}

// OpenLogin shows the in-app Douyin window so the user can log in; the
// page bridge then sends authenticated requests through its cookie store.
func (h *Hub) OpenLogin() {
	h.bridge.ShowLogin()
}

// ---------- followed users ----------

// Users returns the follow list.
func (h *Hub) Users() []*FollowedUser {
	return h.users.List()
}

// AddUser registers a profile URL or bare sec_uid; returns the entry.
func (h *Hub) AddUser(text string) (*FollowedUser, error) {
	sec := douyin.SecUIDFromText(text)
	if sec == "" {
		for _, u := range douyin.ExtractURLs(text) {
			resolved, err := h.client.Load().Resolve(context.Background(), u)
			if err != nil {
				continue
			}
			if link := douyin.Classify(resolved); link != nil && link.Type == douyin.TypeUser {
				sec = link.SecUID
				break
			}
		}
	}
	if sec == "" {
		return nil, fmt.Errorf("没有识别到用户主页链接或用户 ID")
	}
	f := h.users.Add(sec)
	_ = UsersChanged.Broadcast(h.users.List())
	return f, nil
}

// RemoveUser drops a followed user.
func (h *Hub) RemoveUser(secUID string) bool {
	if !h.users.Remove(secUID) {
		return false
	}
	_ = UsersChanged.Broadcast(h.users.List())
	return true
}

// RefreshUser fetches the user's newest posts and returns them marked with
// New flags; the list doubles as the pick UI's data source. It pages until
// it reaches an aweme seen by an earlier refresh (bounded so first-time
// checks don't fetch a whole posting history).
func (h *Hub) RefreshUser(secUID string) (*UserRefresh, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	seen := h.users.seenSet(secUID)
	var awemes []map[string]any
	var cursor int64
	for page := 0; page < 4; page++ {
		p, err := h.client.Load().GetUserPost(ctx, secUID, cursor)
		if err != nil {
			if page == 0 {
				return nil, err
			}
			break // later-page failure keeps what we already fetched
		}
		awemes = append(awemes, p.Items...)
		hitSeen := false
		for _, aw := range p.Items {
			if seen[douyin.AwemeID(aw)] {
				hitSeen = true
				break
			}
		}
		if !p.HasMore || hitSeen {
			break
		}
		cursor = p.MaxCursor
	}
	res, err := h.users.RecordRefresh(secUID, awemes)
	if err != nil {
		return nil, err
	}
	_ = UsersChanged.Broadcast(h.users.List())
	return res, nil
}

// DownloadUserItems queues the checked awemes of a followed user's last
// refresh as a prepared download job.
func (h *Hub) DownloadUserItems(secUID string, itemIDs []string) (string, error) {
	awemes := h.users.AwemeMaps(secUID, itemIDs)
	if len(awemes) == 0 {
		return "", fmt.Errorf("请先在该用户卡片上「检查更新」再选择下载")
	}
	title := "主页 " + secUID
	if nick := h.users.nickname(secUID); nick != "" {
		title = nick + " 的更新"
	}
	job := h.manager.SubmitPrepared(title, "https://www.douyin.com/user/"+secUID, awemes)
	return job.ID, nil
}

// follow registers a profile job's user in the follow list so they show up
// on the users view; silently no-ops when already followed.
func (h *Hub) follow(link douyin.Link) {
	if link.Type != douyin.TypeUser || link.SecUID == "" {
		return
	}
	before := len(h.users.List())
	h.users.Add(link.SecUID)
	if len(h.users.List()) != before {
		_ = UsersChanged.Broadcast(h.users.List())
	}
}

// Submit parses pasted share text, resolves links and creates jobs.
// Resolving short links needs network I/O, which is fine: bound calls run
// on their own goroutine.
func (h *Hub) Submit(text string) (SubmitResult, error) {
	urls := douyin.ExtractURLs(text)
	if len(urls) == 0 {
		if uid := douyin.SecUIDFromText(text); uid != "" {
			link := douyin.Link{
				Type:    douyin.TypeUser,
				URL:     "https://www.douyin.com/user/" + uid,
				SecUID:  uid,
				Display: "主页 " + uid,
			}
			h.follow(link)
			job := h.manager.Submit(link)
			return SubmitResult{Created: []string{job.ID}}, nil
		}
		return SubmitResult{}, fmt.Errorf("没有识别到抖音链接或用户 ID")
	}
	var res SubmitResult
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	for _, u := range urls {
		resolved, err := h.client.Load().Resolve(ctx, u)
		if err != nil {
			res.Failed = append(res.Failed, u+" （短链解析失败: "+err.Error()+"）")
			continue
		}
		link := douyin.Classify(resolved)
		if link == nil {
			res.Skipped = append(res.Skipped, resolved)
			continue
		}
		h.follow(*link)
		job := h.manager.Submit(*link)
		res.Created = append(res.Created, job.ID)
	}
	if len(res.Created) == 0 && len(res.Failed) == 0 && len(res.Skipped) > 0 {
		return res, fmt.Errorf("链接类型暂不支持或无法识别")
	}
	return res, nil
}

// Jobs returns the current job table.
func (h *Hub) Jobs() []*queue.Job {
	return h.manager.Snapshot()
}

// RetryJob requeues a finished/failed job.
func (h *Hub) RetryJob(id string) bool {
	return h.manager.Retry(id)
}

// PickJob downloads the checked items and mixes of an awaiting-pick job.
func (h *Hub) PickJob(id string, itemIDs, mixIDs []string) bool {
	return h.manager.Pick(id, itemIDs, mixIDs)
}

// RemoveJob deletes a finished job.
func (h *Hub) RemoveJob(id string) bool {
	return h.manager.Remove(id)
}

// ClearFinished removes all done/failed jobs.
func (h *Hub) ClearFinished() int {
	return h.manager.ClearFinished()
}

// RevealItem opens the file manager at a downloaded file.
func (h *Hub) RevealItem(path string) {
	mygo.Shell.ShowItemInFolder(path)
}

// HubStatus summarizes engine state for the UI chrome.
type HubStatus struct {
	DownloadDir string `json:"downloadDir"`
	HasCookie   bool   `json:"hasCookie"`
	Active      int    `json:"active"`
	Failed      int    `json:"failed"`
	JobCount    int    `json:"jobCount"`
}

// Status summarizes engine state for the UI chrome.
func (h *Hub) Status() HubStatus {
	s := h.settings.Get()
	jobs := h.manager.Snapshot()
	st := HubStatus{DownloadDir: s.DownloadDir, HasCookie: s.Cookie != "", JobCount: len(jobs)}
	for _, j := range jobs {
		switch j.Status {
		case queue.StatusDone, queue.StatusSkipped:
		case queue.StatusFailed:
			st.Failed++
		default:
			st.Active++
		}
	}
	return st
}
