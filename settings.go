package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/egoist/mygo"
	"vhub/internal/douyin"
)

// SettingsData is the user-facing configuration, persisted as JSON in the
// app's user data directory.
type SettingsData struct {
	DownloadDir string  `json:"downloadDir"`
	Cookie      string  `json:"cookie"`
	Proxy       string  `json:"proxy"`
	Quality     string  `json:"quality"`  // "highest" | "lowest"
	Workers     int     `json:"workers"`  // concurrent item downloads
	RateLimit   float64 `json:"rateLimit"` // API requests per second
}

// Settings is the bound settings service. Changes rebuild the API client so
// cookie/proxy edits take effect immediately.
type Settings struct {
	mu       sync.Mutex
	path     string
	data     SettingsData
	onUpdate func(SettingsData)
}

// LoadSettings reads the saved configuration, applying defaults.
func LoadSettings(onUpdate func(SettingsData)) (*Settings, error) {
	dir, err := mygo.App.Path(mygo.PathUserData)
	if err != nil {
		return nil, err
	}
	dl, _ := mygo.App.Path(mygo.PathDownloads)
	s := &Settings{
		path: filepath.Join(dir, "settings.json"),
		data: SettingsData{
			DownloadDir: filepath.Join(dl, "VHub"),
			Quality:     string(douyin.QualityHighest),
			Workers:     3,
			RateLimit:   2,
		},
		onUpdate: onUpdate,
	}
	if raw, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(raw, &s.data)
	}
	if s.data.Workers <= 0 {
		s.data.Workers = 3
	}
	if s.data.RateLimit <= 0 {
		s.data.RateLimit = 2
	}
	if s.data.Quality == "" {
		s.data.Quality = string(douyin.QualityHighest)
	}
	return s, nil
}

// Get returns the current settings.
func (s *Settings) Get() SettingsData {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data
}

// Update saves settings and rebuilds the API client.
func (s *Settings) Update(next SettingsData) error {
	if next.DownloadDir == "" {
		return errors.New("下载目录不能为空")
	}
	if next.Workers <= 0 {
		next.Workers = 3
	}
	if next.RateLimit <= 0 {
		next.RateLimit = 2
	}
	if next.Quality != string(douyin.QualityLowest) {
		next.Quality = string(douyin.QualityHighest)
	}
	raw, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(s.path, raw, 0o600); err != nil {
		return err
	}
	s.mu.Lock()
	s.data = next
	s.mu.Unlock()
	if s.onUpdate != nil {
		s.onUpdate(next)
	}
	return nil
}

// PickDownloadDir opens a native directory picker; "" when canceled.
func (s *Settings) PickDownloadDir(ctx context.Context) (string, error) {
	paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
		Parent:            mygo.CallerWindow(ctx),
		Title:             "选择下载目录",
		Directory:         true,
		CreateDirectories: true,
	})
	if err != nil || len(paths) == 0 {
		return "", err
	}
	return paths[0], nil
}

// OpenDownloadDir reveals the download directory in the file manager.
func (s *Settings) OpenDownloadDir() error {
	dir := s.Get().DownloadDir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return mygo.Shell.OpenPath(dir)
}
