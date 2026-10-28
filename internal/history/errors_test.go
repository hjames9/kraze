package history

import (
	"context"
	"errors"
	"os"
	"os/user"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestLoadAndDeleteErrors(t *testing.T) {
	ctx := context.Background()
	failing := func(verb string) *fake.Clientset {
		cs := fake.NewSimpleClientset()
		cs.PrependReactor(verb, "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("forbidden")
		})
		return cs
	}
	if _, err := Load(ctx, failing("get")); err == nil {
		t.Error("a ConfigMap that can't be read is an error")
	}
	corrupt := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: ConfigMapNamespace},
		Data:       map[string]string{ConfigMapDataKey: "{not json"},
	})
	if _, err := Load(ctx, corrupt); err == nil {
		t.Error("history that doesn't parse is an error")
	}
	if err := Delete(ctx, fake.NewSimpleClientset()); err != nil {
		t.Errorf("deleting no history is fine, got %v", err)
	}
	if err := Delete(ctx, failing("delete")); err == nil {
		t.Error("a delete that fails is an error")
	}
}

// $USER, then the OS's user, then "unknown"; "@host" when the hostname is known
func TestCurrentUser(t *testing.T) {
	t.Cleanup(func() { lookupUser, hostname = user.Current, os.Hostname })
	osUser := func(name string, err error) func() (*user.User, error) {
		return func() (*user.User, error) { return &user.User{Username: name}, err }
	}
	host := func(name string, err error) func() (string, error) {
		return func() (string, error) { return name, err }
	}
	failed := errors.New("unknown")
	for _, tt := range []struct {
		env    string
		lookup func() (*user.User, error)
		host   func() (string, error)
		want   string
	}{
		{"dev", osUser("os", nil), host("box", nil), "dev@box"},
		{"", osUser("os", nil), host("box", nil), "os@box"},
		{"", osUser("", failed), host("box", nil), "unknown@box"},
		{"dev", osUser("os", nil), host("", failed), "dev"},
		{"dev", osUser("os", nil), host("", nil), "dev"},
	} {
		t.Setenv("USER", tt.env)
		lookupUser, hostname = tt.lookup, tt.host
		if got := currentUser(); got != tt.want {
			t.Errorf("USER=%q: user = %q, want %q", tt.env, got, tt.want)
		}
	}
}

func TestRecordErrors(t *testing.T) {
	ctx := context.Background()
	failing := func(verb string) *fake.Clientset {
		cs := fake.NewSimpleClientset()
		cs.PrependReactor(verb, "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("forbidden")
		})
		return cs
	}
	if _, err := Begin(ctx, failing("get"), Entry{Command: "up"}); err == nil {
		t.Error("history that can't be read can't be recorded")
	}
	if _, err := Begin(ctx, failing("create"), Entry{Command: "up"}); err == nil {
		t.Error("history that can't be created can't be recorded")
	}
	// An existing ConfigMap without data is filled in
	empty := fake.NewSimpleClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: ConfigMapNamespace}})
	if _, err := Begin(ctx, empty, Entry{Command: "up"}); err != nil {
		t.Fatal(err)
	}
	if entries, err := Load(ctx, empty); err != nil || len(entries) != 1 {
		t.Errorf("entries = %v, %v", entries, err)
	}
}

func TestRecordUpdateFails(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: ConfigMapNamespace},
		Data:       map[string]string{ConfigMapDataKey: "[]"},
	})
	cs.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	if _, err := Begin(ctx, cs, Entry{Command: "up"}); err == nil {
		t.Error("history that can't be updated can't be recorded")
	}
}
