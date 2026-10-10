package main

import (
	"testing"

	"github.com/egoist/mygo"
)

// Bind validates method signatures eagerly — this catches types that cannot
// cross the IPC boundary without needing a running app.
func TestServicesBind(t *testing.T) {
	s := &Settings{data: SettingsData{DownloadDir: t.TempDir(), Quality: "highest", Workers: 1, RateLimit: 1}}
	h := NewHub(s)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Bind panicked: %v", r)
		}
	}()
	mygo.BindAs("TestHub", h)
	mygo.BindAs("TestSettings", s)
}

func TestSettingsRoundTrip(t *testing.T) {
	s := &Settings{data: SettingsData{DownloadDir: "/tmp/x"}}
	got := s.Get()
	if got.DownloadDir != "/tmp/x" {
		t.Fatal("settings not returned")
	}
}
