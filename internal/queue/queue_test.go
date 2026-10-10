package queue

import (
	"testing"
	"time"

	"vhub/internal/douyin"
)

// A job submitted without a client fails resolving instead of hanging.
func TestSubmitFailsWithoutClient(t *testing.T) {
	m := NewManager(Options{Workers: 1})
	job := m.Submit(douyin.Link{Type: douyin.TypeVideo, URL: "u", ID: "1"})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if job.Status == StatusFailed {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job did not fail, status %s", job.Status)
}

func TestRetryAndRemoveGuards(t *testing.T) {
	m := NewManager(Options{Workers: 1})
	job := m.Submit(douyin.Link{Type: douyin.TypeVideo, URL: "u", ID: "1"})
	deadline := time.Now().Add(3 * time.Second)
	for job.Status != StatusFailed && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !m.Retry(job.ID) {
		t.Fatal("retry of failed job rejected")
	}
	for job.Status != StatusFailed && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !m.Remove(job.ID) {
		t.Fatal("remove of failed job rejected")
	}
	if m.Get(job.ID) != nil {
		t.Fatal("job still present after remove")
	}
}

// seedAwaitingPick installs a job already in the pick state without going
// through Submit (which needs a live client).
func seedAwaitingPick(m *Manager, id string, items ...*Item) *Job {
	job := &Job{
		ID: id, Status: StatusAwaitingPick,
		link:  douyin.Link{Type: douyin.TypeUser},
		Items: items,
	}
	m.mu.Lock()
	m.jobs = append(m.jobs, job)
	m.byID[job.ID] = job
	m.mu.Unlock()
	return job
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("timed out waiting for %s", what)
	}
}

// Pick marks unchecked items skipped and resumes the job; the checked item
// has no media candidates offline so it lands failed.
func TestPickResumesAndSkips(t *testing.T) {
	m := NewManager(Options{Workers: 1})
	job := seedAwaitingPick(m, "j1",
		&Item{ID: "a", Status: StatusPending},
		&Item{ID: "b", Status: StatusPending})

	if !m.Pick(job.ID, []string{"a"}, nil) {
		t.Fatal("pick rejected for awaiting_pick job")
	}
	waitFor(t, func() bool { return job.Status == StatusFailed }, "job to finish")
	if job.Items[1].Status != StatusSkipped || job.Skipped != 1 {
		t.Fatalf("unchecked item not skipped: status=%s skipped=%d", job.Items[1].Status, job.Skipped)
	}
	if job.Items[0].Status != StatusFailed {
		t.Fatalf("picked item was not attempted: %s", job.Items[0].Status)
	}
}

// Picking nothing leaves every item skipped and the job marked skipped,
// which also makes it removable.
func TestPickNothingMarksJobSkipped(t *testing.T) {
	m := NewManager(Options{Workers: 1})
	job := seedAwaitingPick(m, "j2",
		&Item{ID: "a", Status: StatusPending},
		&Item{ID: "b", Status: StatusPending})

	m.Pick(job.ID, nil, nil)
	waitFor(t, func() bool { return job.Status == StatusSkipped }, "job to be skipped")
	if job.Skipped != 2 {
		t.Fatalf("skipped=%d, want 2", job.Skipped)
	}
	if !m.Remove(job.ID) {
		t.Fatal("cancelled (skipped) job not removable")
	}
}

// Pick on a job that is not waiting for selection is a no-op.
func TestPickRejectsOtherStatuses(t *testing.T) {
	m := NewManager(Options{Workers: 1})
	job := seedAwaitingPick(m, "j3", &Item{ID: "a", Status: StatusPending})
	job.Status = StatusPending
	if m.Pick(job.ID, []string{"a"}, nil) {
		t.Fatal("pick accepted for non-awaiting job")
	}
	if m.Get("nope") != nil || m.Pick("nope", nil, nil) {
		t.Fatal("pick accepted for unknown job")
	}
}

func TestSnapshotIsolation(t *testing.T) {
	m := NewManager(Options{Workers: 1})
	job := m.Submit(douyin.Link{Type: douyin.TypeVideo, URL: "u", ID: "1"})
	snap := m.Snapshot()
	snap[0].Status = StatusDone
	if job.Status == StatusDone {
		t.Fatal("snapshot mutation leaked into live job")
	}
}
