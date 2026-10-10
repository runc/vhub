// Package queue runs download jobs: it resolves what a link contains
// (one aweme, a profile's posts, a mix, a music page) and downloads each
// item with progress reporting.
package queue

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"vhub/internal/douyin"
)

// Status is the state of a job or one of its items.
type Status string

const (
	StatusPending      Status = "pending"
	StatusResolving    Status = "resolving" // fetching detail / listing pages
	StatusAwaitingPick Status = "awaiting_pick" // items listed, waiting for selection
	StatusDownloading  Status = "downloading"
	StatusDone         Status = "done"
	StatusFailed       Status = "failed"
	StatusSkipped      Status = "skipped"
)

// Item is one downloadable work piece (a video or an image of a gallery).
type Item struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Kind       string `json:"kind"` // "video" | "gallery"
	Cover      string `json:"cover,omitempty"` // poster/thumbnail URL for the pick UI
	Time       int64  `json:"time,omitempty"`  // publish time, unix seconds
	Path       string `json:"path,omitempty"`
	BytesDone  int64  `json:"bytesDone"`
	BytesTotal int64  `json:"bytesTotal"`
	Status     Status `json:"status"`
	Error      string `json:"error,omitempty"`

	urls      []douyin.Candidate // resolved media candidates
	awemeData map[string]any
}

// MixEntry is one of a profile's collections, listed for picking on
// user jobs. Selected mixes expand into items at download time.
type MixEntry struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Count int    `json:"count"`
	Cover string `json:"cover,omitempty"`
}

// Job is a user-submitted link turned into a unit of work.
type Job struct {
	ID         string      `json:"id"`
	URL        string      `json:"url"`
	Type       string      `json:"type"`
	Title      string      `json:"title"`
	Status     Status      `json:"status"`
	Items      []*Item     `json:"items"`
	Mixes      []*MixEntry `json:"mixes,omitempty"`
	Total      int         `json:"total"`
	Done       int         `json:"done"`
	Failed     int         `json:"failed"`
	Skipped    int         `json:"skipped"`
	Error      string      `json:"error,omitempty"`
	CreatedAt  time.Time   `json:"createdAt"`
	FinishedAt time.Time   `json:"finishedAt,omitempty"`

	link          douyin.Link
	pendingMixIDs []string // mixes checked in the pick UI, expanded on resume
	prepared      bool     // Items pre-resolved by the caller — skip collect
}

// Options configure a Manager.
type Options struct {
	Client      *douyin.Client
	DownloadDir string
	Quality     douyin.Quality
	Workers     int
	// OnChange is invoked with a full snapshot whenever any job state
	// changes; it may be called from any goroutine.
	OnChange func([]*Job)
}

// Manager owns the job table and the concurrency gate.
type Manager struct {
	mu   sync.Mutex
	jobs []*Job
	byID map[string]*Job
	seq  int

	// Mutable execution config, swapped by Update.
	cfgMu       sync.Mutex
	client      *douyin.Client
	downloadDir string
	quality     douyin.Quality
	sem         chan struct{}
	onChange    func([]*Job)

	broadcastMu sync.Mutex
	pending     bool
}

// NewManager creates the job manager.
func NewManager(opts Options) *Manager {
	m := &Manager{byID: map[string]*Job{}}
	m.Update(opts)
	return m
}

// Update swaps the execution config; in-flight jobs keep running.
func (m *Manager) Update(opts Options) {
	if opts.Workers <= 0 {
		opts.Workers = 3
	}
	m.cfgMu.Lock()
	defer m.cfgMu.Unlock()
	if opts.Client != nil {
		m.client = opts.Client
	}
	if opts.DownloadDir != "" {
		m.downloadDir = opts.DownloadDir
	}
	if opts.Quality != "" {
		m.quality = opts.Quality
	}
	m.sem = make(chan struct{}, opts.Workers)
	if opts.OnChange != nil {
		m.onChange = opts.OnChange
	}
}

func (m *Manager) cfg() (client *douyin.Client, dir string, q douyin.Quality) {
	m.cfgMu.Lock()
	defer m.cfgMu.Unlock()
	return m.client, m.downloadDir, m.quality
}

// Submit creates a job for a parsed link.
func (m *Manager) Submit(link douyin.Link) *Job {
	m.mu.Lock()
	m.seq++
	job := &Job{
		ID:        fmt.Sprintf("j%05d", m.seq),
		URL:       link.URL,
		Type:      string(link.Type),
		Title:     link.Display,
		Status:    StatusPending,
		CreatedAt: time.Now(),
		link:      link,
	}
	m.jobs = append(m.jobs, job)
	m.byID[job.ID] = job
	m.mu.Unlock()
	m.dispatch(job)
	m.emit()
	return job
}

// SubmitPrepared creates a job whose items are already resolved — the
// caller passes aweme dicts (e.g. picked from a followed user's refresh)
// and the job goes straight to downloading, skipping collect and the
// pick UI.
func (m *Manager) SubmitPrepared(title, url string, awemes []map[string]any) *Job {
	var items []*Item
	for _, aw := range awemes {
		items = append(items, m.itemFromAweme(aw))
	}
	m.mu.Lock()
	m.seq++
	job := &Job{
		ID:        fmt.Sprintf("j%05d", m.seq),
		URL:       url,
		Type:      string(douyin.TypeUser),
		Title:     title,
		Status:    StatusPending,
		Items:     items,
		Total:     len(items),
		CreatedAt: time.Now(),
		prepared:  true,
		link:      douyin.Link{Type: douyin.TypeUser, URL: url},
	}
	m.jobs = append(m.jobs, job)
	m.byID[job.ID] = job
	m.mu.Unlock()
	m.dispatch(job)
	m.emit()
	return job
}

// clone deep-copies a job for snapshots; callers hold m.mu.
func (j *Job) clone() *Job {
	c := *j
	c.link = douyin.Link{} // runtime-only detail, not needed by the UI
	c.pendingMixIDs = nil
	if j.Mixes != nil {
		c.Mixes = append([]*MixEntry(nil), j.Mixes...)
	}
	if j.Items != nil {
		c.Items = make([]*Item, len(j.Items))
		for i, it := range j.Items {
			ic := *it
			ic.urls = nil
			ic.awemeData = nil
			c.Items[i] = &ic
		}
	}
	return &c
}

// Snapshot returns a deep copy of the job table, safe to marshal while
// workers keep mutating the live jobs.
func (m *Manager) Snapshot() []*Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Job, len(m.jobs))
	for i, j := range m.jobs {
		out[i] = j.clone()
	}
	return out
}

// Get returns one job by id.
func (m *Manager) Get(id string) *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.byID[id]
}

// Retry requeues a failed job.
func (m *Manager) Retry(id string) bool {
	job := m.Get(id)
	if job == nil || (job.Status != StatusFailed && job.Status != StatusDone && job.Status != StatusSkipped) {
		return false
	}
	m.mu.Lock()
	job.Status = StatusPending
	job.Error = ""
	job.FinishedAt = time.Time{}
	job.Total, job.Done, job.Failed, job.Skipped = 0, 0, 0, 0
	job.Items = nil
	job.Mixes = nil
	job.pendingMixIDs = nil
	m.mu.Unlock()
	m.dispatch(job)
	m.emit()
	return true
}

// Remove deletes a finished job.
func (m *Manager) Remove(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, j := range m.jobs {
		if j.ID != id {
			continue
		}
		if j.Status != StatusDone && j.Status != StatusFailed &&
			j.Status != StatusSkipped && j.Status != StatusAwaitingPick {
			return false
		}
		m.jobs = append(m.jobs[:i], m.jobs[i+1:]...)
		delete(m.byID, id)
		go m.emit()
		return true
	}
	return false
}

// ClearFinished removes all done/failed jobs.
func (m *Manager) ClearFinished() int {
	m.mu.Lock()
	n := 0
	kept := m.jobs[:0]
	for _, j := range m.jobs {
		if j.Status == StatusDone || j.Status == StatusFailed || j.Status == StatusSkipped {
			delete(m.byID, j.ID)
			n++
			continue
		}
		kept = append(kept, j)
	}
	m.jobs = kept
	m.mu.Unlock()
	if n > 0 {
		m.emit()
	}
	return n
}

// emit asks the OnChange callback to run, throttled so rapid progress
// updates do not flood the UI (progress events are coalesced at ~8/s).
func (m *Manager) emit() {
	m.cfgMu.Lock()
	onChange := m.onChange
	m.cfgMu.Unlock()
	if onChange == nil {
		return
	}
	m.broadcastMu.Lock()
	if m.pending {
		m.broadcastMu.Unlock()
		return
	}
	m.pending = true
	m.broadcastMu.Unlock()
	time.AfterFunc(120*time.Millisecond, func() {
		m.broadcastMu.Lock()
		m.pending = false
		m.broadcastMu.Unlock()
		onChange(m.Snapshot())
	})
}

// dispatch runs a job as soon as a worker slot is free. The semaphore is
// swapped by Update, so its capacity always reflects the current setting.
func (m *Manager) dispatch(job *Job) {
	m.cfgMu.Lock()
	sem := m.sem
	m.cfgMu.Unlock()
	go func() {
		sem <- struct{}{}
		defer func() { <-sem }()
		m.run(job)
	}()
}

// run resolves the job's items then downloads them one by one. User jobs
// pause in awaiting_pick after listing their posts/collections until Pick
// resumes them; re-entering with that status skips straight to downloading.
func (m *Manager) run(job *Job) {
	m.mu.Lock()
	resume := job.Status == StatusAwaitingPick
	prepared := job.prepared
	m.mu.Unlock()
	if prepared {
		m.download(job)
		return
	}
	if resume {
		m.resumePicked(job)
		return
	}

	m.setStatus(job, StatusResolving)
	ctx := context.Background()
	items, err := m.collect(ctx, job)
	if err != nil {
		m.failJob(job, err)
		return
	}
	var mixes []*MixEntry
	if job.link.Type == douyin.TypeUser {
		mixes = m.collectMixes(ctx, job)
	}
	if len(items) == 0 && len(mixes) == 0 {
		m.failJob(job, errors.New("没有可下载的内容"))
		return
	}
	m.mu.Lock()
	job.Items = items
	job.Mixes = mixes
	job.Total = len(items)
	m.mu.Unlock()
	m.emit()

	if job.link.Type == douyin.TypeUser {
		m.setStatus(job, StatusAwaitingPick)
		return
	}
	m.download(job)
}

// Pick confirms the checked items and mixes of an awaiting_pick job, marks
// the rest skipped, and requeues the job for download.
func (m *Manager) Pick(id string, itemIDs, mixIDs []string) bool {
	job := m.Get(id)
	if job == nil || job.Status != StatusAwaitingPick {
		return false
	}
	want := map[string]bool{}
	for _, id := range itemIDs {
		want[id] = true
	}
	m.mu.Lock()
	for _, it := range job.Items {
		if it.Status != StatusPending {
			continue
		}
		if !want[it.ID] {
			it.Status = StatusSkipped
			job.Skipped++
		}
	}
	job.pendingMixIDs = append([]string(nil), mixIDs...)
	m.mu.Unlock()
	m.dispatch(job)
	m.emit()
	return true
}

// resumePicked expands the checked mixes into items, then downloads
// everything still pending.
func (m *Manager) resumePicked(job *Job) {
	if len(job.pendingMixIDs) > 0 {
		m.setStatus(job, StatusResolving)
		ctx := context.Background()
		for _, mixID := range job.pendingMixIDs {
			m.expandMix(ctx, job, mixID)
		}
	}
	m.mu.Lock()
	job.pendingMixIDs = nil
	job.Total = len(job.Items)
	pending := 0
	for _, it := range job.Items {
		if it.Status == StatusPending {
			pending++
		}
	}
	m.mu.Unlock()
	if pending == 0 {
		m.finish(job)
		return
	}
	m.download(job)
}

// expandMix fetches a checked collection's awemes and appends them to the
// job. Awemes already listed (from posts) are unskipped instead of
// duplicated; a mix that fails entirely shows up as one failed item.
func (m *Manager) expandMix(ctx context.Context, job *Job, mixID string) {
	c, _, _ := m.cfg()
	if c == nil {
		return
	}
	title := mixID
	for _, mx := range job.Mixes {
		if mx.ID == mixID {
			title = mx.Title
			break
		}
	}
	sub, err := m.collectPaged(job, func(cursor int64) ([]map[string]any, bool, int64, error) {
		p, err := c.GetMixAweme(ctx, mixID, cursor)
		return p.Items, p.HasMore, p.MaxCursor, err
	})
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(sub) == 0 && err != nil {
		job.Items = append(job.Items, &Item{
			ID: "mix:" + mixID, Title: "合集·" + title, Kind: "video",
			Status: StatusFailed, Error: err.Error(),
		})
		job.Failed++
		return
	}
	byID := map[string]*Item{}
	for _, it := range job.Items {
		byID[it.ID] = it
	}
	for _, it := range sub {
		if ex := byID[it.ID]; ex != nil {
			if ex.Status == StatusSkipped {
				ex.Status = StatusPending
				job.Skipped--
			}
			continue
		}
		byID[it.ID] = it
		job.Items = append(job.Items, it)
	}
}

// download runs every pending item, then finalizes the job status.
func (m *Manager) download(job *Job) {
	m.setStatus(job, StatusDownloading)
	for _, it := range job.Items {
		if it.Status == StatusPending {
			m.downloadItem(job, it)
		}
	}
	m.finish(job)
}

func (m *Manager) finish(job *Job) {
	m.mu.Lock()
	switch {
	case job.Failed > 0:
		job.Status = StatusFailed
		job.Error = fmt.Sprintf("%d/%d 个项目下载失败", job.Failed, job.Total)
	case job.Done == 0:
		job.Status = StatusSkipped
	default:
		job.Status = StatusDone
	}
	job.FinishedAt = time.Now()
	m.mu.Unlock()
	m.emit()
}

// collect expands a link into the aweme items to download.
func (m *Manager) collect(ctx context.Context, job *Job) ([]*Item, error) {
	c, _, _ := m.cfg()
	if c == nil {
		return nil, errors.New("no API client configured")
	}
	switch job.link.Type {
	case douyin.TypeVideo, douyin.TypeGallery:
		detail, err := c.GetVideoDetail(ctx, job.link.ID)
		if err != nil {
			return nil, err
		}
		return []*Item{m.itemFromAweme(detail)}, nil

	case douyin.TypeUser:
		return m.collectPaged(job, func(cursor int64) ([]map[string]any, bool, int64, error) {
			p, err := c.GetUserPost(ctx, job.link.SecUID, cursor)
			return p.Items, p.HasMore, p.MaxCursor, err
		})

	case douyin.TypeMix:
		return m.collectPaged(job, func(cursor int64) ([]map[string]any, bool, int64, error) {
			p, err := c.GetMixAweme(ctx, job.link.ID, cursor)
			return p.Items, p.HasMore, p.MaxCursor, err
		})

	case douyin.TypeMusic:
		return m.collectPaged(job, func(cursor int64) ([]map[string]any, bool, int64, error) {
			p, err := c.GetMusicAweme(ctx, job.link.ID, cursor)
			return p.Items, p.HasMore, p.MaxCursor, err
		})
	}
	return nil, fmt.Errorf("unsupported link type %q", job.link.Type)
}

// collectPaged walks a cursor-paginated list endpoint with stall detection.
func (m *Manager) collectPaged(
	job *Job,
	fetch func(cursor int64) (items []map[string]any, hasMore bool, next int64, err error),
) ([]*Item, error) {
	var out []*Item
	seen := map[string]bool{}
	var cursor int64
	for page := 0; page < 50; page++ {
		awemes, hasMore, next, err := fetch(cursor)
		if err != nil {
			if len(out) > 0 {
				return out, nil // keep what we got
			}
			return nil, err
		}
		for _, aw := range awemes {
			if id := douyin.AwemeID(aw); id != "" && !seen[id] {
				seen[id] = true
				out = append(out, m.itemFromAweme(aw))
			}
		}
		if !hasMore || next <= cursor {
			break // end of list or stalled cursor
		}
		cursor = next
	}
	return out, nil
}

// collectMixes lists a profile's collections for the pick UI. Best-effort:
// failures keep whatever pages arrived so posts can still be downloaded.
func (m *Manager) collectMixes(ctx context.Context, job *Job) []*MixEntry {
	c, _, _ := m.cfg()
	if c == nil {
		return nil
	}
	var out []*MixEntry
	seen := map[string]bool{}
	var cursor int64
	for page := 0; page < 50; page++ {
		p, err := c.GetUserMix(ctx, job.link.SecUID, cursor)
		if err != nil {
			break
		}
		for _, e := range p.Items {
			info := douyin.MixInfoOf(e)
			if info.ID == "" || seen[info.ID] {
				continue
			}
			seen[info.ID] = true
			if info.Name == "" {
				info.Name = "合集 " + info.ID
			}
			out = append(out, &MixEntry{ID: info.ID, Title: info.Name, Count: info.Count, Cover: info.Cover})
		}
		if !p.HasMore || p.MaxCursor <= cursor {
			break
		}
		cursor = p.MaxCursor
	}
	return out
}

var unsafeNameChars = regexp.MustCompile(`[\\/:*?"<>|\x00-\x1f]`)

func sanitizeName(s string, fallback string) string {
	s = unsafeNameChars.ReplaceAllString(s, " ")
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) > 60 {
		s = string([]rune(s)[:60])
	}
	if s == "" {
		return fallback
	}
	return s
}

// itemFromAweme turns an aweme detail dict into a download Item with its
// media candidates resolved.
func (m *Manager) itemFromAweme(aw map[string]any) *Item {
	id := douyin.AwemeID(aw)
	title := sanitizeName(douyin.AwemeTitle(aw), id)
	kind := douyin.DetectMediaKind(aw)
	it := &Item{ID: id, Title: title, Status: StatusPending, awemeData: aw, Cover: douyin.AwemeCover(aw), Time: douyin.AwemeTime(aw)}
	author := sanitizeName(douyin.AwemeAuthorName(aw), "douyin")
	c, baseDir, quality := m.cfg()
	dir := filepath.Join(baseDir, author)

	if kind == douyin.KindGallery {
		imageSets := douyin.GalleryImageURLs(aw)
		if len(imageSets) == 0 {
			it.Kind = "video" // note with a video track instead
			it.urls = c.VideoCandidates(aw, quality)
			it.Path = filepath.Join(dir, fmt.Sprintf("%s_%s.mp4", id, title))
		} else {
			it.Kind = "gallery"
			it.Path = dir // expanded per image at download time
		}
	} else {
		it.Kind = "video"
		it.urls = c.VideoCandidates(aw, quality)
		it.Path = filepath.Join(dir, fmt.Sprintf("%s_%s.mp4", id, title))
	}
	return it
}

// downloadItem downloads one item, trying each media candidate in turn.
func (m *Manager) downloadItem(job *Job, it *Item) {
	m.setItemStatus(job, it, StatusDownloading)
	ctx := context.Background()
	c, _, _ := m.cfg()

	progress := func(done, total int64) {
		m.mu.Lock()
		it.BytesDone, it.BytesTotal = done, total
		m.mu.Unlock()
		m.emit()
	}

	if it.Kind == "gallery" {
		m.downloadGallery(ctx, job, it, progress)
		return
	}
	if len(it.urls) == 0 {
		m.failItem(job, it, errors.New("没有可用的下载地址"))
		return
	}
	var lastErr error
	for _, cand := range it.urls {
		if err := c.DownloadFile(ctx, cand, it.Path, progress); err != nil {
			lastErr = err
			continue
		}
		m.mu.Lock()
		it.Status = StatusDone
		job.Done++
		m.mu.Unlock()
		m.emit()
		return
	}
	m.failItem(job, it, fmt.Errorf("所有候选地址均失败: %w", lastErr))
}

// downloadGallery saves every image of a gallery aweme.
func (m *Manager) downloadGallery(ctx context.Context, job *Job, it *Item, progress func(done, total int64)) {
	c, _, _ := m.cfg()
	sets := douyin.GalleryImageURLs(it.awemeData)
	if len(sets) == 0 {
		m.failItem(job, it, errors.New("图集为空"))
		return
	}
	failed := 0
	for i, urls := range sets {
		dst := filepath.Join(it.Path, fmt.Sprintf("%s_%s_%02d.jpg", it.ID, it.Title, i+1))
		saved := false
		var lastErr error
		for _, u := range urls {
			cand := douyin.Candidate{URL: u, Headers: c.DownloadHeaders("")}
			if err := c.DownloadFile(ctx, cand, dst, progress); err == nil {
				saved = true
				break
			} else {
				lastErr = err
			}
		}
		if !saved {
			failed++
			_ = lastErr
		}
	}
	m.mu.Lock()
	if failed == len(sets) {
		it.Status = StatusFailed
		it.Error = "所有图片下载失败"
		job.Failed++
	} else {
		it.Status = StatusDone
		if failed > 0 {
			it.Error = fmt.Sprintf("%d/%d 张图片失败", failed, len(sets))
		}
		job.Done++
	}
	m.mu.Unlock()
	m.emit()
}

func (m *Manager) setStatus(job *Job, s Status) {
	m.mu.Lock()
	job.Status = s
	m.mu.Unlock()
	m.emit()
}

func (m *Manager) setItemStatus(job *Job, it *Item, s Status) {
	m.mu.Lock()
	it.Status = s
	m.mu.Unlock()
	m.emit()
}

func (m *Manager) failJob(job *Job, err error) {
	m.mu.Lock()
	job.Status = StatusFailed
	job.Error = err.Error()
	job.FinishedAt = time.Now()
	m.mu.Unlock()
	m.emit()
}

func (m *Manager) failItem(job *Job, it *Item, err error) {
	m.mu.Lock()
	it.Status = StatusFailed
	it.Error = err.Error()
	job.Failed++
	m.mu.Unlock()
	m.emit()
}
