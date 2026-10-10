package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/egoist/mygo"
	"vhub/internal/douyin"
)

// FollowedUser is a Douyin profile the app tracks for new posts.
type FollowedUser struct {
	SecUID    string    `json:"secUID"`
	Nickname  string    `json:"nickname"`
	Avatar    string    `json:"avatar,omitempty"`
	AddedAt   time.Time `json:"addedAt"`
	LastCheck time.Time `json:"lastCheck,omitempty"`
	// SeenIDs are aweme ids already surfaced to the user — newest first,
	// capped so the file stays small. Posts pinned to the top of a profile
	// keep their old id, which is why this is a set rather than a watermark.
	SeenIDs []string `json:"seenIDs,omitempty"`
}

// UserItem is one recent post offered for picking on the users view.
type UserItem struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Kind  string `json:"kind"`
	Cover string `json:"cover,omitempty"`
	Time  int64  `json:"time,omitempty"` // publish time, unix seconds
	New   bool   `json:"new"`            // not seen by an earlier refresh
}

// UserRefresh is what RefreshUser returns to the UI.
type UserRefresh struct {
	User  *FollowedUser `json:"user"`
	Items []*UserItem   `json:"items"`
}

const maxSeenIDs = 300

// Users is the bound user-tracking service: a persisted follow list plus a
// per-user aweme cache so DownloadUserItems can rebuild items without
// re-fetching.
type Users struct {
	mu     sync.Mutex
	path   string
	list   []*FollowedUser
	awemes map[string]map[string]map[string]any // secUID → awemeID → aweme
}

// LoadUsers reads the persisted follow list, creating an empty one if the
// file is missing or unreadable.
func LoadUsers() (*Users, error) {
	dir, err := mygo.App.Path(mygo.PathUserData)
	if err != nil {
		return nil, err
	}
	u := &Users{
		path:   filepath.Join(dir, "users.json"),
		awemes: map[string]map[string]map[string]any{},
	}
	if raw, err := os.ReadFile(u.path); err == nil {
		_ = json.Unmarshal(raw, &u.list)
	}
	return u, nil
}

// List returns the follow list, oldest first.
func (u *Users) List() []*FollowedUser {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]*FollowedUser(nil), u.list...)
}

// Add registers a sec_uid; returns the (new or existing) entry.
func (u *Users) Add(secUID string) *FollowedUser {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, f := range u.list {
		if f.SecUID == secUID {
			return f
		}
	}
	f := &FollowedUser{SecUID: secUID, AddedAt: time.Now()}
	u.list = append(u.list, f)
	_ = u.saveLocked()
	return f
}

// Remove drops a user and its aweme cache.
func (u *Users) Remove(secUID string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	for i, f := range u.list {
		if f.SecUID != secUID {
			continue
		}
		u.list = append(u.list[:i], u.list[i+1:]...)
		delete(u.awemes, secUID)
		_ = u.saveLocked()
		return true
	}
	return false
}

// seenSet returns a copy of the user's seen aweme ids (nil for unknown
// users — treated as "nothing seen yet").
func (u *Users) seenSet(secUID string) map[string]bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	f := u.get(secUID)
	if f == nil {
		return nil
	}
	seen := map[string]bool{}
	for _, id := range f.SeenIDs {
		seen[id] = true
	}
	return seen
}

// nickname returns the user's display name under the lock ("" if unknown).
func (u *Users) nickname(secUID string) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if f := u.get(secUID); f != nil {
		return f.Nickname
	}
	return ""
}

// get reads a user entry; callers hold u.mu.
func (u *Users) get(secUID string) *FollowedUser {
	for _, f := range u.list {
		if f.SecUID == secUID {
			return f
		}
	}
	return nil
}

// RecordRefresh marks the fetched awemes seen, stores them for later
// downloads, updates nickname/avatar, and returns the pick list with New
// flags applied.
func (u *Users) RecordRefresh(secUID string, awemes []map[string]any) (*UserRefresh, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	f := u.get(secUID)
	if f == nil {
		return nil, errors.New("未关注该用户")
	}
	seen := map[string]bool{}
	for _, id := range f.SeenIDs {
		seen[id] = true
	}
	cache := u.awemes[secUID]
	if cache == nil {
		cache = map[string]map[string]any{}
	}
	fresh := []string{}
	res := &UserRefresh{User: f}
	for _, aw := range awemes {
		id := douyin.AwemeID(aw)
		if id == "" {
			continue
		}
		cache[id] = aw
		if name := douyin.AwemeAuthorName(aw); name != "" {
			f.Nickname = name
		}
		if av := douyin.AwemeAuthorAvatar(aw); av != "" {
			f.Avatar = av
		}
		title := douyin.AwemeTitle(aw)
		if title == "" {
			title = id
		}
		isNew := !seen[id]
		seen[id] = true // dedupe repeats across pages within one refresh
		it := &UserItem{
			ID:    id,
			Title: title,
			Kind:  string(douyin.DetectMediaKind(aw)),
			Cover: douyin.AwemeCover(aw),
			Time:  douyin.AwemeTime(aw),
			New:   isNew,
		}
		if it.New {
			fresh = append(fresh, id)
		}
		res.Items = append(res.Items, it)
	}
	u.awemes[secUID] = cache
	f.SeenIDs = append(fresh, f.SeenIDs...)
	if len(f.SeenIDs) > maxSeenIDs {
		f.SeenIDs = f.SeenIDs[:maxSeenIDs]
	}
	f.LastCheck = time.Now()
	_ = u.saveLocked()
	return res, nil
}

// AwemeMaps returns cached aweme data for the given ids (nil entries for
// ids not seen in the latest refresh).
func (u *Users) AwemeMaps(secUID string, ids []string) []map[string]any {
	u.mu.Lock()
	defer u.mu.Unlock()
	cache := u.awemes[secUID]
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		if aw := cache[id]; aw != nil {
			out = append(out, aw)
		}
	}
	return out
}

func (u *Users) saveLocked() error {
	raw, err := json.MarshalIndent(u.list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(u.path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(u.path, raw, 0o600)
}
