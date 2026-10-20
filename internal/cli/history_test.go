package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/hjames9/kraze/internal/history"
)

func TestSelectHistory(t *testing.T) {
	entries := []history.Entry{
		{ID: "1", Changes: []history.Change{{Name: "redis"}}},
		{ID: "2", Changes: []history.Change{{Name: "backend"}, {Name: "redis"}}},
		{ID: "3", Changes: []history.Change{{Name: "frontend"}}},
		{ID: "4"},
	}

	tests := []struct {
		name     string
		services []string
		limit    int
		want     []string
	}{
		{name: "all, newest first", want: []string{"4", "3", "2", "1"}},
		{name: "limited", limit: 2, want: []string{"4", "3"}},
		{name: "filtered by service", services: []string{"redis"}, want: []string{"2", "1"}},
		{name: "filtered by any of several services", services: []string{"frontend", "backend"}, want: []string{"3", "2"}},
		{name: "filtered and limited", services: []string{"redis"}, limit: 1, want: []string{"2"}},
		{name: "no match", services: []string{"missing"}, want: []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := selectHistory(entries, tt.services, "", true, tt.limit)
			ids := make([]string, 0, len(got))
			for _, e := range got {
				ids = append(ids, e.ID)
			}
			if strings.Join(ids, ",") != strings.Join(tt.want, ",") {
				t.Errorf("Expected entries %v, got %v", tt.want, ids)
			}
		})
	}
}

func TestSelectHistoryByProject(t *testing.T) {
	entries := []history.Entry{
		{ID: "1"},
		{ID: "2", Project: "billing"},
		{ID: "3", Project: "web"},
		{ID: "4", Project: "billing"},
	}

	tests := []struct {
		name        string
		project     string
		allProjects bool
		want        string
	}{
		{name: "a project sees its own and pre-project entries", project: "billing", want: "4,2,1"},
		{name: "all projects", project: "billing", allProjects: true, want: "4,3,2,1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ids []string
			for _, e := range selectHistory(entries, nil, tt.project, tt.allProjects, 0) {
				ids = append(ids, e.ID)
			}
			if got := strings.Join(ids, ","); got != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestFormatHistoryProjectColumn(t *testing.T) {
	entries := []history.Entry{
		{Command: "up", Project: "billing", Result: history.ResultSucceeded},
		{Command: "up", Result: history.ResultSucceeded},
	}
	out := formatHistory(entries, false, true)
	for _, want := range []string{"PROJECT", "billing", "(no project)"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in:\n%s", want, out)
		}
	}
	if strings.Contains(formatHistory(entries, false, false), "PROJECT") {
		t.Error("PROJECT column should only appear with --all-projects")
	}
}

func TestSummarizeChanges(t *testing.T) {
	e := history.Entry{
		ClusterCreated: true,
		Changes: []history.Change{
			{Name: "redis", Action: history.ActionInstall, Result: history.ResultSucceeded},
			{Name: "backend", Action: history.ActionUpgrade, Result: history.ResultSucceeded},
			{Name: "worker", Action: history.ActionUninstall, Result: history.ResultSucceeded},
			{Name: "nginx:1.27", Action: history.ActionLoadImage, Result: history.ResultSucceeded},
			{Name: "api", Action: history.ActionInstall, Result: history.ResultFailed},
			{Name: "db", Action: history.ActionUpgrade, Result: history.ResultInterrupted},
		},
	}

	want := "(cluster created) +redis ~backend -worker ^nginx:1.27 ✗api !db"
	if got := summarizeChanges(e); got != want {
		t.Errorf("Expected %q, got %q", want, got)
	}
}

func TestFormatHistory(t *testing.T) {
	entries := []history.Entry{{
		StartedAt: time.Date(2026, 9, 19, 14, 2, 11, 0, time.UTC),
		User:      "dev@devbox",
		Command:   "up",
		Args:      "up backend --no-deps",
		Result:    history.ResultRunning,
		Changes: []history.Change{
			{Name: "backend", Action: history.ActionUpgrade, Result: history.ResultFailed, Error: "timed out", DurationMs: 1500},
		},
	}}

	out := formatHistory(entries, true, false)

	for _, want := range []string{"dev@devbox", "up backend --no-deps", "incomplete", "✗backend", "1.5s", "error: timed out"} {
		if !strings.Contains(out, want) {
			t.Errorf("Expected output to contain %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "running") {
		t.Errorf("Expected running entries to be shown as incomplete, got:\n%s", out)
	}

	if strings.Contains(formatHistory(entries, false, false), "error: timed out") {
		t.Error("Expected per-change details only in verbose mode")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("Expected short string unchanged, got %q", got)
	}
	if got := truncate("up backend frontend --no-deps", 10); got != "up back..." {
		t.Errorf("Expected truncation with ellipsis, got %q", got)
	}
	if got := truncate("héllo wörld", 8); got != "héllo..." {
		t.Errorf("Expected rune-safe truncation, got %q", got)
	}
}
