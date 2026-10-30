package logging

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
)

type partialTimelineFailure struct{ file *os.File }

func (w partialTimelineFailure) Write(data []byte) (int, error) {
	n, _ := w.file.Write(data[:min(2, len(data))])
	_ = w.file.Close()
	return n, errors.New("synthetic write failure")
}

func TestFileBodySourceTimelineFailureCommittedPrefix(t *testing.T) {
	s, err := NewFileBodySourceInDir(t.TempDir(), "timeline")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Cleanup() }()
	if err := s.AppendTimelinePart([]byte("prefix")); err != nil {
		t.Fatal(err)
	}
	s.timelineWriter = partialTimelineFailure{s.timelineFile}
	if err := s.AppendTimelinePart([]byte("failed event")); err == nil {
		t.Fatal("partial write did not fail")
	}
	got, err := s.Bytes()
	if err != nil || string(got) != "prefix\n" {
		t.Fatalf("committed prefix=%q err=%v", got, err)
	}
	if s.timelineFile != nil {
		t.Fatal("failed file descriptor retained")
	}
}

func TestFileBodySourceTimelineMixedPartsAndCleanup(t *testing.T) {
	s, err := NewFileBodySourceInDir(t.TempDir(), "timeline")
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		timeline bool
		data     string
	}{{true, "one"}, {true, "two"}, {false, "other"}, {true, "three"}} {
		var err error
		if step.timeline {
			err = s.AppendTimelinePart([]byte(step.data))
		} else {
			err = s.AppendPart([]byte(step.data))
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Bytes()
	if err != nil || string(got) != "one\n\ntwo\n\nother\n\nthree\n" {
		t.Fatalf("ordered parts=%q err=%v", got, err)
	}
	file := s.timelineFile
	paths := s.Paths()
	if err := s.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("closed")); err == nil {
		t.Fatal("cleanup retained file descriptor")
	}
	for _, path := range paths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("cleanup retained part")
		}
	}
	if err := s.AppendTimelinePart([]byte("late")); err == nil {
		t.Fatal("append after cleanup succeeded")
	}
}

func TestFileBodySourceTimelineManualClearRecordsNewEvents(t *testing.T) {
	s, err := NewFileBodySourceInDir(t.TempDir(), "timeline")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Cleanup() }()
	if err := s.AppendTimelinePart([]byte("explicitly deleted")); err != nil {
		t.Fatal(err)
	}
	old := s.timelineFile
	if err := os.RemoveAll(s.dir); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendTimelinePart([]byte("after clear")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Bytes()
	if err != nil || string(got) != "after clear\n" {
		t.Fatalf("manual-clear bytes=%q err=%v", got, err)
	}
	if _, err := old.Write([]byte("closed")); err == nil {
		t.Fatal("unlinked descriptor retained")
	}
}

func TestFileBodySourceTimelineConcurrentAppend(t *testing.T) {
	s, err := NewFileBodySourceInDir(t.TempDir(), "timeline")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Cleanup() }()
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.AppendTimelinePart([]byte("event")); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got, err := s.Bytes()
	if err != nil || strings.Count(string(got), "event") != 64 || len(s.Paths()) != 1 {
		t.Fatal("concurrent append lost events or created extra files")
	}
}
