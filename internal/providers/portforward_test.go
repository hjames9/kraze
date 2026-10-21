package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/streaming/pkg/httpstream"
	spdystream "k8s.io/streaming/pkg/httpstream/spdy"
)

// PortForward up to the stream itself: the stream needs a real API server
// speaking SPDY, so a successful forward isn't tested here
func TestPortForwardFailures(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		kubeconfig string
		ports      []string
		want       string
	}{
		"kubeconfig doesn't parse": {"not: [yaml", []string{"80"}, "failed to parse kubeconfig"},
		"no cluster in it":         {"apiVersion: v1\nkind: Config\ncurrent-context: none\n", []string{"80"}, "failed to create REST config"},
		"bad server URL":           {strings.Replace(unreachableKubeconfig, "https://127.0.0.1:1", "https://bad host", 1), []string{"80"}, "failed to create Kubernetes client"},
		"bad port":                 {offlineKubeconfig, []string{"not-a-port"}, "failed to create port forwarder"},
		"cluster unreachable":      {offlineKubeconfig, []string{"80"}, "port forward failed"},
	} {
		t.Run(name, func(t *testing.T) {
			err := PortForward(ctx, tc.kubeconfig, "apps", "web-1", tc.ports)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// Cancelled while the API server hasn't answered: stops and reports it
func TestPortForwardCancelledWhileConnecting(t *testing.T) {
	stop := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-stop:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(stop)
	kubeconfig := strings.Replace(unreachableKubeconfig, "https://127.0.0.1:1", server.URL, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := PortForward(ctx, kubeconfig, "apps", "web-1", []string{"18080:80"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the deadline", err)
	}
}

// fakePortForwardServer is an API server that accepts the port-forward
// upgrade, as a kubelet would, without carrying any traffic. upgraded is
// closed once a client has connected; with dropAfter, the server drops the
// connection that long after the upgrade.
func fakePortForwardServer(t *testing.T, dropAfter time.Duration) (kubeconfig string, upgraded <-chan struct{}) {
	t.Helper()
	connected := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := httpstream.Handshake(r, w, []string{"portforward.k8s.io"}); err != nil {
			return
		}
		conn := spdystream.NewResponseUpgrader().UpgradeResponse(w, r, func(httpstream.Stream, <-chan struct{}) error { return nil })
		if conn == nil {
			return
		}
		once.Do(func() { close(connected) })
		if dropAfter > 0 {
			time.Sleep(dropAfter)
			_ = conn.Close()
			return
		}
		<-conn.CloseChan()
	}))
	t.Cleanup(server.Close)
	return strings.Replace(unreachableKubeconfig, "https://127.0.0.1:1", server.URL, 1), connected
}

// Once the forward is set up, it runs until the command is cancelled
func TestPortForwardRunsUntilCancelled(t *testing.T) {
	kubeconfig, upgraded := fakePortForwardServer(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- PortForward(ctx, kubeconfig, "apps", "web-1", []string{":80"}) }()

	select {
	case <-upgraded:
	case err := <-done:
		t.Fatalf("PortForward ended before connecting: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("PortForward didn't connect")
	}
	// The forwarder reports ready right after it starts listening locally
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("a cancelled forward that was running isn't an error, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PortForward didn't stop after cancellation")
	}
}

// A connection the API server drops while forwarding ends the forward with an
// error
func TestPortForwardConnectionLost(t *testing.T) {
	kubeconfig, _ := fakePortForwardServer(t, 200*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := PortForward(ctx, kubeconfig, "apps", "web-1", []string{":80"}); err == nil || !strings.Contains(err.Error(), "port forward failed") {
		t.Errorf("err = %v", err)
	}
}
