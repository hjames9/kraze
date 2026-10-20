// Package history records the commands that changed a cluster (kraze up, down,
// init, load-image) in a ConfigMap alongside the cluster state, so anyone with
// access to the cluster can see what changed, when, and by whom.
//
// History is informational, not a security audit trail: the user field is taken
// from the local environment and is trivially spoofable.
package history

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

const (
	// ConfigMapName is the name of the ConfigMap storing kraze history. It is kept
	// separate from the state ConfigMap so state loads stay small and history can
	// never push state over the 1 MiB object size limit.
	ConfigMapName = "kraze-history"

	// ConfigMapNamespace is the namespace where the history ConfigMap is stored
	ConfigMapNamespace = "kube-system"

	// ConfigMapDataKey is the key in the ConfigMap data field
	ConfigMapDataKey = "history"

	// MaxEntries is the number of entries retained; the oldest are dropped first
	MaxEntries = 200

	// MaxErrorLength bounds recorded error messages (Helm errors can be very long)
	MaxErrorLength = 1024

	// finishTimeout bounds the final write, which runs even after the command's
	// context has been cancelled (Ctrl-C)
	finishTimeout = 10 * time.Second
)

// Result is the outcome of a command or of a single change within it
type Result string

const (
	// ResultRunning means the command has started but not recorded a final result.
	// An entry left in this state belongs to a run that is still going or that was
	// killed before it could finish (SIGKILL, double Ctrl-C, crash).
	ResultRunning     Result = "running"
	ResultSucceeded   Result = "succeeded"
	ResultFailed      Result = "failed"
	ResultInterrupted Result = "interrupted"
)

// Entry records a single kraze command run against the cluster
type Entry struct {
	ID             string     `json:"id"`
	StartedAt      time.Time  `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	User           string     `json:"user"`
	Project        string     `json:"project,omitempty"` // kraze.yml project key; empty for anonymous configs
	KrazeVersion   string     `json:"kraze_version,omitempty"`
	Command        string     `json:"command"`        // up, down, init, load-image
	Args           string     `json:"args,omitempty"` // full command line, e.g. "up backend --no-deps"
	ClusterCreated bool       `json:"cluster_created,omitempty"`
	Result         Result     `json:"result"`
	Error          string     `json:"error,omitempty"`
	Changes        []Change   `json:"changes,omitempty"`
}

// Change records one service or image operation within a command
type Change struct {
	Name       string `json:"name"`   // service name or image reference
	Action     string `json:"action"` // install, upgrade, uninstall, load-image
	Result     Result `json:"result"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"duration_ms"`
}

// Actions recorded in Change.Action
const (
	ActionInstall   = "install"
	ActionUpgrade   = "upgrade"
	ActionUninstall = "uninstall"
	ActionLoadImage = "load-image"
)

// Recorder records one command run. It writes a running entry when it begins and
// replaces it with the final result on Finish. All methods are safe on a nil
// Recorder and safe for concurrent use.
type Recorder struct {
	ctx       context.Context
	clientset kubernetes.Interface

	mu       sync.Mutex
	entry    Entry
	finished bool
}

// Begin starts recording a command. The template supplies Command, Args,
// KrazeVersion and ClusterCreated; ID, StartedAt, User and Result are filled in.
//
// The running entry is written immediately so that a run killed before Finish
// still leaves a trace. The returned Recorder is always usable: if this first
// write fails, the error is returned and Finish tries again.
func Begin(ctx context.Context, clientset kubernetes.Interface, template Entry) (*Recorder, error) {
	entry := template
	entry.ID = newID()
	entry.StartedAt = time.Now().UTC()
	entry.Result = ResultRunning
	if entry.User == "" {
		entry.User = currentUser()
	}

	r := &Recorder{ctx: ctx, clientset: clientset, entry: entry}
	return r, upsert(ctx, clientset, entry)
}

// RecordChange records the outcome of one service or image operation. It only
// updates memory; the change is written to the cluster by Finish.
func (r *Recorder) RecordChange(name, action string, duration time.Duration, err error) {
	if r == nil {
		return
	}
	change := Change{
		Name:       name,
		Action:     action,
		Result:     resultFor(err, false),
		Error:      truncateError(err),
		DurationMs: duration.Milliseconds(),
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.entry.Changes = append(r.entry.Changes, change)
}

// Finish records the final result of the command, replacing its running entry.
// It is idempotent and writes with a context detached from the command's, so the
// result is still recorded after Ctrl-C has cancelled the command.
func (r *Recorder) Finish(err error) error {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	if r.finished {
		r.mu.Unlock()
		return nil
	}
	r.finished = true

	now := time.Now().UTC()
	r.entry.FinishedAt = &now
	r.entry.Result = resultFor(err, r.ctx.Err() != nil)
	r.entry.Error = truncateError(err)
	if r.entry.Result == ResultSucceeded {
		// Commands like down report per-service failures without returning an error
		for _, c := range r.entry.Changes {
			if c.Result != ResultSucceeded {
				r.entry.Result = c.Result
				break
			}
		}
	}
	entry := r.entry
	entry.Changes = append([]Change(nil), r.entry.Changes...)
	r.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), finishTimeout)
	defer cancel()
	return upsert(ctx, r.clientset, entry)
}

// Load returns the recorded history, oldest first. It returns nil if no history
// has been recorded.
func Load(ctx context.Context, clientset kubernetes.Interface) ([]Entry, error) {
	cm, err := clientset.CoreV1().ConfigMaps(ConfigMapNamespace).Get(ctx, ConfigMapName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read history ConfigMap: %w", err)
	}
	return decode(cm.Data[ConfigMapDataKey])
}

// Delete removes the history ConfigMap from the cluster
func Delete(ctx context.Context, clientset kubernetes.Interface) error {
	err := clientset.CoreV1().ConfigMaps(ConfigMapNamespace).Delete(ctx, ConfigMapName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete history ConfigMap: %w", err)
	}
	return nil
}

// upsert replaces the entry with the same ID, or appends it, and prunes the
// oldest entries beyond MaxEntries. Each attempt re-reads the ConfigMap, so
// concurrent kraze runs that race on the update retry instead of losing entries.
func upsert(ctx context.Context, clientset kubernetes.Interface, entry Entry) error {
	if clientset == nil {
		return nil
	}
	configMaps := clientset.CoreV1().ConfigMaps(ConfigMapNamespace)

	retriable := func(err error) bool {
		return apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err)
	}
	err := retry.OnError(retry.DefaultRetry, retriable, func() error {
		cm, err := configMaps.Get(ctx, ConfigMapName, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		create := err != nil

		var entries []Entry
		if !create {
			// A corrupt history is not worth failing a command over; start afresh
			entries, _ = decode(cm.Data[ConfigMapDataKey])
		}
		data, err := encode(merge(entries, entry))
		if err != nil {
			return err
		}

		if create {
			_, err = configMaps.Create(ctx, &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      ConfigMapName,
					Namespace: ConfigMapNamespace,
					Labels: map[string]string{
						"app.kubernetes.io/managed-by": "kraze",
					},
				},
				Data: map[string]string{ConfigMapDataKey: data},
			}, metav1.CreateOptions{})
			return err
		}

		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[ConfigMapDataKey] = data
		_, err = configMaps.Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("failed to write history: %w", err)
	}
	return nil
}

// merge replaces the entry with a matching ID or appends it, keeping at most
// MaxEntries of the newest entries
func merge(entries []Entry, entry Entry) []Entry {
	replaced := false
	for i := range entries {
		if entries[i].ID == entry.ID {
			entries[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		entries = append(entries, entry)
	}
	if len(entries) > MaxEntries {
		entries = entries[len(entries)-MaxEntries:]
	}
	return entries
}

func decode(data string) ([]Entry, error) {
	if data == "" {
		return nil, nil
	}
	var entries []Entry
	if err := json.Unmarshal([]byte(data), &entries); err != nil {
		return nil, fmt.Errorf("failed to parse history: %w", err)
	}
	return entries, nil
}

func encode(entries []Entry) (string, error) {
	data, err := json.Marshal(entries)
	if err != nil {
		return "", fmt.Errorf("failed to marshal history: %w", err)
	}
	return string(data), nil
}

// resultFor maps an error to a Result. interrupted reports whether the command's
// context was cancelled, which catches commands that swallow per-service errors.
func resultFor(err error, interrupted bool) Result {
	switch {
	case errors.Is(err, context.Canceled) || (err != nil && interrupted):
		return ResultInterrupted
	case err != nil:
		return ResultFailed
	case interrupted:
		return ResultInterrupted
	default:
		return ResultSucceeded
	}
}

func truncateError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) <= MaxErrorLength {
		return msg
	}
	// Cutting at a byte offset can split a multi-byte rune; drop the fragment
	return strings.ToValidUTF8(msg[:MaxErrorLength], "") + "…"
}

func currentUser() string {
	name := os.Getenv("USER")
	if name == "" {
		if u, err := user.Current(); err == nil {
			name = u.Username
		}
	}
	if name == "" {
		name = "unknown"
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		return name + "@" + host
	}
	return name
}

func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
