package history

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func mustLoad(t *testing.T, clientset *fake.Clientset) []Entry {
	t.Helper()
	entries, err := Load(context.Background(), clientset)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	return entries
}

func TestBeginWritesRunningEntry(t *testing.T) {
	clientset := fake.NewSimpleClientset()

	rec, err := Begin(context.Background(), clientset, Entry{Command: "up", Args: "up backend", KrazeVersion: "1.2.3"})
	if err != nil {
		t.Fatalf("Begin failed: %v", err)
	}

	entries := mustLoad(t, clientset)
	if len(entries) != 1 {
		t.Fatalf("Expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.ID != rec.entry.ID || e.ID == "" {
		t.Errorf("Expected entry ID %q, got %q", rec.entry.ID, e.ID)
	}
	if e.Result != ResultRunning {
		t.Errorf("Expected result %q, got %q", ResultRunning, e.Result)
	}
	if e.Command != "up" || e.Args != "up backend" || e.KrazeVersion != "1.2.3" {
		t.Errorf("Template fields not preserved: %+v", e)
	}
	if e.User == "" || e.StartedAt.IsZero() || e.FinishedAt != nil {
		t.Errorf("Expected user and start time set, finish time unset: %+v", e)
	}
}

func TestFinishReplacesEntry(t *testing.T) {
	tests := []struct {
		name    string
		changes []error
		err     error
		cancel  bool
		want    Result
	}{
		{name: "success", changes: []error{nil, nil}, want: ResultSucceeded},
		{name: "command error", err: errors.New("boom"), want: ResultFailed},
		{name: "failed change without command error", changes: []error{nil, errors.New("boom")}, want: ResultFailed},
		{name: "cancelled error", err: fmt.Errorf("install: %w", context.Canceled), want: ResultInterrupted},
		{name: "context cancelled, error swallowed", changes: []error{nil}, cancel: true, want: ResultInterrupted},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := fake.NewSimpleClientset()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			rec, err := Begin(ctx, clientset, Entry{Command: "down"})
			if err != nil {
				t.Fatalf("Begin failed: %v", err)
			}
			for i, changeErr := range tt.changes {
				rec.RecordChange(fmt.Sprintf("svc-%d", i), ActionUninstall, time.Second, changeErr)
			}
			if tt.cancel {
				cancel()
			}
			if err := rec.Finish(tt.err); err != nil {
				t.Fatalf("Finish failed: %v", err)
			}

			entries := mustLoad(t, clientset)
			if len(entries) != 1 {
				t.Fatalf("Expected Finish to replace the running entry, got %d entries", len(entries))
			}
			e := entries[0]
			if e.Result != tt.want {
				t.Errorf("Expected result %q, got %q", tt.want, e.Result)
			}
			if e.FinishedAt == nil {
				t.Error("Expected FinishedAt to be set")
			}
			if len(e.Changes) != len(tt.changes) {
				t.Errorf("Expected %d changes, got %d", len(tt.changes), len(e.Changes))
			}
			if tt.err != nil && e.Error != tt.err.Error() {
				t.Errorf("Expected error %q, got %q", tt.err.Error(), e.Error)
			}
		})
	}
}

func TestFinishWritesAfterContextCancelled(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	ctx, cancel := context.WithCancel(context.Background())

	rec, _ := Begin(ctx, clientset, Entry{Command: "up"})
	cancel()

	if err := rec.Finish(context.Canceled); err != nil {
		t.Fatalf("Finish failed after cancellation: %v", err)
	}
	if got := mustLoad(t, clientset)[0].Result; got != ResultInterrupted {
		t.Errorf("Expected result %q, got %q", ResultInterrupted, got)
	}
}

func TestFinishIsIdempotent(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	rec, _ := Begin(context.Background(), clientset, Entry{Command: "up"})

	if err := rec.Finish(nil); err != nil {
		t.Fatalf("Finish failed: %v", err)
	}
	if err := rec.Finish(errors.New("late")); err != nil {
		t.Fatalf("second Finish failed: %v", err)
	}
	if got := mustLoad(t, clientset)[0].Result; got != ResultSucceeded {
		t.Errorf("Expected second Finish to be ignored, got result %q", got)
	}
}

func TestFinishRecordsEntryWhenBeginFailed(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	failCreate := true
	clientset.PrependReactor("create", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if failCreate {
			return true, nil, errors.New("apiserver unavailable")
		}
		return false, nil, nil
	})

	rec, err := Begin(context.Background(), clientset, Entry{Command: "up"})
	if err == nil {
		t.Fatal("Expected Begin to report the failed write")
	}
	if rec == nil {
		t.Fatal("Expected a usable Recorder even when Begin fails")
	}

	failCreate = false
	if err := rec.Finish(nil); err != nil {
		t.Fatalf("Finish failed: %v", err)
	}
	if entries := mustLoad(t, clientset); len(entries) != 1 || entries[0].Result != ResultSucceeded {
		t.Errorf("Expected Finish to append the entry, got %+v", entries)
	}
}

func TestNilRecorderIsSafe(t *testing.T) {
	var rec *Recorder
	rec.RecordChange("svc", ActionInstall, time.Second, nil)
	if err := rec.Finish(errors.New("boom")); err != nil {
		t.Errorf("Expected nil Recorder Finish to return nil, got %v", err)
	}
}

func TestNilClientsetIsSafe(t *testing.T) {
	rec, err := Begin(context.Background(), nil, Entry{Command: "up"})
	if err != nil {
		t.Fatalf("Begin with nil clientset failed: %v", err)
	}
	rec.RecordChange("svc", ActionInstall, time.Second, nil)
	if err := rec.Finish(nil); err != nil {
		t.Errorf("Finish with nil clientset failed: %v", err)
	}
}

func TestConcurrentRecordChange(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	rec, _ := Begin(context.Background(), clientset, Entry{Command: "up"})

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec.RecordChange(fmt.Sprintf("svc-%d", i), ActionInstall, time.Second, nil)
		}()
	}
	wg.Wait()

	if err := rec.Finish(nil); err != nil {
		t.Fatalf("Finish failed: %v", err)
	}
	if got := len(mustLoad(t, clientset)[0].Changes); got != 20 {
		t.Errorf("Expected 20 changes, got %d", got)
	}
}

func TestUpsertRetriesOnConflict(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	if _, err := Begin(context.Background(), clientset, Entry{Command: "init"}); err != nil {
		t.Fatalf("Begin failed: %v", err)
	}

	conflicts := 0
	clientset.PrependReactor("update", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts < 2 {
			conflicts++
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, ConfigMapName, errors.New("modified"))
		}
		return false, nil, nil
	})

	if _, err := Begin(context.Background(), clientset, Entry{Command: "up"}); err != nil {
		t.Fatalf("Begin failed despite retries: %v", err)
	}
	if conflicts != 2 {
		t.Errorf("Expected 2 conflicts to be retried, got %d", conflicts)
	}
	if got := len(mustLoad(t, clientset)); got != 2 {
		t.Errorf("Expected both entries to be recorded, got %d", got)
	}
}

func TestUpsertRetriesWhenCreateRaces(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	raced := false
	clientset.PrependReactor("create", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if raced {
			return false, nil, nil
		}
		raced = true
		// Another kraze run creates the ConfigMap between our Get and Create
		other := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: ConfigMapNamespace},
			Data:       map[string]string{ConfigMapDataKey: `[{"id":"other","command":"down","result":"running"}]`},
		}
		if err := clientset.Tracker().Add(other); err != nil {
			t.Fatalf("failed to simulate concurrent create: %v", err)
		}
		return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "configmaps"}, ConfigMapName)
	})

	if _, err := Begin(context.Background(), clientset, Entry{Command: "up"}); err != nil {
		t.Fatalf("Begin failed despite retry: %v", err)
	}
	if !raced {
		t.Error("Expected the create race to be exercised")
	}
	if got := len(mustLoad(t, clientset)); got != 2 {
		t.Errorf("Expected the concurrent entry to be kept alongside ours, got %d", got)
	}
}

func TestMergePrunesOldestEntries(t *testing.T) {
	var entries []Entry
	for i := range MaxEntries + 5 {
		entries = merge(entries, Entry{ID: fmt.Sprintf("id-%d", i)})
	}

	if len(entries) != MaxEntries {
		t.Fatalf("Expected %d entries, got %d", MaxEntries, len(entries))
	}
	if entries[0].ID != "id-5" {
		t.Errorf("Expected oldest entries to be dropped, first is %q", entries[0].ID)
	}
	if last := entries[len(entries)-1].ID; last != fmt.Sprintf("id-%d", MaxEntries+4) {
		t.Errorf("Expected newest entry last, got %q", last)
	}
}

func TestTruncateError(t *testing.T) {
	if got := truncateError(nil); got != "" {
		t.Errorf("Expected empty string for nil error, got %q", got)
	}
	if got := truncateError(errors.New("short")); got != "short" {
		t.Errorf("Expected short error unchanged, got %q", got)
	}

	// A multi-byte rune straddling the cut must not leave invalid UTF-8
	long := strings.Repeat("a", MaxErrorLength-1) + "é" + strings.Repeat("b", 100)
	got := truncateError(errors.New(long))
	if !utf8.ValidString(got) {
		t.Error("Expected truncated error to be valid UTF-8")
	}
	if len(got) > MaxErrorLength+len("…") {
		t.Errorf("Expected truncated error of at most %d bytes, got %d", MaxErrorLength+len("…"), len(got))
	}
}

func TestLoadAndDeleteWithoutHistory(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	if entries := mustLoad(t, clientset); entries != nil {
		t.Errorf("Expected nil history, got %+v", entries)
	}
	if err := Delete(context.Background(), clientset); err != nil {
		t.Errorf("Expected Delete of missing history to succeed, got %v", err)
	}
}

func TestDelete(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	if _, err := Begin(context.Background(), clientset, Entry{Command: "up"}); err != nil {
		t.Fatalf("Begin failed: %v", err)
	}
	if err := Delete(context.Background(), clientset); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if entries := mustLoad(t, clientset); entries != nil {
		t.Errorf("Expected history to be gone, got %+v", entries)
	}
}

func TestCorruptHistoryIsReplaced(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	if _, err := Begin(context.Background(), clientset, Entry{Command: "up"}); err != nil {
		t.Fatalf("Begin failed: %v", err)
	}
	cm, _ := clientset.CoreV1().ConfigMaps(ConfigMapNamespace).Get(context.Background(), ConfigMapName, metav1.GetOptions{})
	cm.Data[ConfigMapDataKey] = "{not json"
	if _, err := clientset.CoreV1().ConfigMaps(ConfigMapNamespace).Update(context.Background(), cm, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("failed to corrupt history: %v", err)
	}

	if _, err := Load(context.Background(), clientset); err == nil {
		t.Error("Expected Load to report corrupt history")
	}
	if _, err := Begin(context.Background(), clientset, Entry{Command: "down"}); err != nil {
		t.Fatalf("Expected Begin to overwrite corrupt history, got %v", err)
	}
	if got := mustLoad(t, clientset); len(got) != 1 || got[0].Command != "down" {
		t.Errorf("Expected fresh history with one entry, got %+v", got)
	}
}
