package dedupe

import (
	"path/filepath"
	"testing"
	"time"
)

func TestMessageDeduperPersistsCompletedMessagesAcrossInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dedupe.json")
	now := time.Now()

	first := New(path, time.Minute)
	if !first.Begin("feishu-default:om-1", now) {
		t.Fatal("first message should be accepted")
	}
	first.Complete("feishu-default:om-1", now)

	reloaded := New(path, time.Minute)
	if reloaded.Begin("feishu-default:om-1", now.Add(30*time.Second)) {
		t.Fatal("completed message should be rejected after deduper reload")
	}
	if !reloaded.Begin("feishu-default:om-1", now.Add(2*time.Minute)) {
		t.Fatal("expired message should be accepted")
	}
}

func TestMessageDeduperForgetsFailedMessagesAcrossInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dedupe.json")
	now := time.Now()

	first := New(path, time.Minute)
	if !first.Begin("ilink-default:42", now) {
		t.Fatal("first message should be accepted")
	}
	first.Forget("ilink-default:42")

	reloaded := New(path, time.Minute)
	if !reloaded.Begin("ilink-default:42", now.Add(time.Second)) {
		t.Fatal("forgotten message should be accepted after deduper reload")
	}
}
