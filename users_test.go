package main

import "testing"

func newTestUsers() *Users {
	// empty path → saveLocked writes fail silently; fine for unit tests
	return &Users{awemes: map[string]map[string]map[string]any{}}
}

func aweme(id, desc string) map[string]any {
	return map[string]any{"aweme_id": id, "desc": desc}
}

func TestUsersAddDedupAndRemove(t *testing.T) {
	u := newTestUsers()
	f1 := u.Add("sec-a")
	f2 := u.Add("sec-a")
	if f1 != f2 {
		t.Fatal("Add should return the existing entry for a duplicate sec_uid")
	}
	if n := len(u.List()); n != 1 {
		t.Fatalf("expected 1 user, got %d", n)
	}
	if !u.Remove("sec-a") || u.Remove("sec-a") {
		t.Fatal("Remove should return true once, false after")
	}
	if n := len(u.List()); n != 0 {
		t.Fatalf("expected empty list, got %d", n)
	}
}

func TestRecordRefreshMarksNew(t *testing.T) {
	u := newTestUsers()
	u.Add("sec-a")
	r1, err := u.RecordRefresh("sec-a", []map[string]any{aweme("1", "a"), aweme("2", "b")})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range r1.Items {
		if !it.New {
			t.Fatalf("first refresh: item %s should be new", it.ID)
		}
	}
	// second refresh: id 2 seen → not new; id 3 unseen → new
	r2, err := u.RecordRefresh("sec-a", []map[string]any{aweme("3", "c"), aweme("2", "b")})
	if err != nil {
		t.Fatal(err)
	}
	var it2, it3 *UserItem
	for _, it := range r2.Items {
		switch it.ID {
		case "2":
			it2 = it
		case "3":
			it3 = it
		}
	}
	if it2 == nil || it3 == nil {
		t.Fatal("expected items 2 and 3 in second refresh")
	}
	if it2.New {
		t.Fatal("already-seen aweme must not be marked new")
	}
	if !it3.New {
		t.Fatal("unseen aweme should be marked new")
	}
	if u.List()[0].LastCheck.IsZero() {
		t.Fatal("LastCheck should be set after refresh")
	}
}

func TestRecordRefreshMergesCache(t *testing.T) {
	u := newTestUsers()
	u.Add("sec-a")
	if _, err := u.RecordRefresh("sec-a", []map[string]any{aweme("1", "a")}); err != nil {
		t.Fatal(err)
	}
	if _, err := u.RecordRefresh("sec-a", []map[string]any{aweme("2", "b")}); err != nil {
		t.Fatal(err)
	}
	got := u.AwemeMaps("sec-a", []string{"1", "2", "missing"})
	if len(got) != 2 {
		t.Fatalf("cache should keep awemes across refreshes, got %d", len(got))
	}
}

func TestRecordRefreshUnknownUser(t *testing.T) {
	u := newTestUsers()
	if _, err := u.RecordRefresh("ghost", []map[string]any{aweme("1", "a")}); err == nil {
		t.Fatal("refreshing an unfollowed user should error")
	}
}

func TestRemoveDropsCache(t *testing.T) {
	u := newTestUsers()
	u.Add("sec-a")
	if _, err := u.RecordRefresh("sec-a", []map[string]any{aweme("1", "a")}); err != nil {
		t.Fatal(err)
	}
	u.Remove("sec-a")
	if got := u.AwemeMaps("sec-a", []string{"1"}); len(got) != 0 {
		t.Fatal("Remove should drop the aweme cache")
	}
}
