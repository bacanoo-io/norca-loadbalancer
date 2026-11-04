// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package registrar

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/api"
)

type fakeStore struct {
	mu       sync.Mutex
	objects  map[string]*api.CustomObject
	statuses map[string]map[string]any
	beats    int
	applies  int
}

func newFakeStore() *fakeStore {
	return &fakeStore{objects: map[string]*api.CustomObject{}, statuses: map[string]map[string]any{}}
}

func (s *fakeStore) Apply(_ context.Context, obj *api.CustomObject) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[obj.Name] = obj
	s.applies++
	return nil
}

func (s *fakeStore) UpdateStatus(_ context.Context, name string, status map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.objects[name]; !ok {
		return ErrNotFound
	}
	s.statuses[name] = status
	s.beats++
	return nil
}

func (s *fakeStore) Delete(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, name)
	return nil
}

func (s *fakeStore) snapshot() (objects map[string]*api.CustomObject, beats, applies int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]*api.CustomObject{}
	for k, v := range s.objects {
		out[k] = v
	}
	return out, s.beats, s.applies
}

var testReg = Registration{
	Class:         "nginx-edge",
	Endpoint:      "https://lb.example.com:8443/classes/nginx-edge",
	TokenVariable: "lb-provider/nginx-edge/token",
	Internal:      true,
	Ports:         "1024-65535",
	Parameters:    map[string]any{"listen_address": "0.0.0.0"},
}

func TestObject(t *testing.T) {
	obj := testReg.Object()
	if obj.Group != "norca.io" || obj.Kind != "LoadBalancerClass" || obj.Name != "nginx-edge" || obj.Namespace != "" {
		t.Fatalf("identity: %+v", obj)
	}
	if obj.ManagedBy != ManagedBy {
		t.Fatalf("managed by %q", obj.ManagedBy)
	}
	want := `{"capabilities":{"internal":true,"ports":"1024-65535"},"endpoint":"https://lb.example.com:8443/classes/nginx-edge",` +
		`"parameters":{"listen_address":"0.0.0.0"},"tokenVariable":"lb-provider/nginx-edge/token"}`
	got, _ := json.Marshal(obj.Spec)
	if string(got) != want {
		t.Fatalf("spec\n got %s\nwant %s", got, want)
	}

	// Optional parts are left out.
	min := Registration{Class: "x", Endpoint: "https://x"}.Object()
	got, _ = json.Marshal(min.Spec)
	if string(got) != `{"capabilities":{"internal":false},"endpoint":"https://x"}` {
		t.Fatalf("minimal spec: %s", got)
	}
}

func waitFor(t *testing.T, desc string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout: %s", desc)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRunRegistersBeatsAndDeregisters(t *testing.T) {
	store := newFakeStore()
	r := &Registrar{
		Store: store, Registrations: []Registration{testReg},
		Interval: 10 * time.Millisecond, Version: "1.2.3", DeregisterOnExit: true,
		Now: func() time.Time { return time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC) },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	waitFor(t, "registered with heartbeats", func() bool {
		objs, beats, _ := store.snapshot()
		return len(objs) == 1 && beats >= 3
	})
	store.mu.Lock()
	st := store.statuses["nginx-edge"]
	store.mu.Unlock()
	if st["lastHeartbeat"] != "2026-11-04T12:00:00Z" || st["version"] != "1.2.3" {
		t.Fatalf("status: %v", st)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if objs, _, _ := store.snapshot(); len(objs) != 0 {
		t.Fatal("the registration stayed after exit")
	}
}

func TestHeartbeatRegistersAgainWhenTheObjectDisappears(t *testing.T) {
	store := newFakeStore()
	r := &Registrar{Store: store, Registrations: []Registration{testReg}, Interval: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Run(ctx) }()
	waitFor(t, "registered", func() bool { o, _, _ := store.snapshot(); return len(o) == 1 })

	_ = store.Delete(ctx, "nginx-edge") // somebody deleted it
	waitFor(t, "registered again", func() bool {
		o, _, applies := store.snapshot()
		return len(o) == 1 && applies >= 2
	})
}

// failingStore fails the first applies.
type failingStore struct {
	*fakeStore
	failures int
}

func (s *failingStore) Apply(ctx context.Context, obj *api.CustomObject) error {
	s.mu.Lock()
	if s.failures > 0 {
		s.failures--
		s.mu.Unlock()
		return http.ErrHandlerTimeout
	}
	s.mu.Unlock()
	return s.fakeStore.Apply(ctx, obj)
}

func TestRegistrationIsRetried(t *testing.T) {
	store := &failingStore{fakeStore: newFakeStore(), failures: 2}
	r := &Registrar{Store: store, Registrations: []Registration{testReg}, Interval: time.Hour, Retry: 5 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Run(ctx) }()
	waitFor(t, "registered after failures", func() bool { o, _, _ := store.snapshot(); return len(o) == 1 })
}

// TestAPIStore checks the requests against the shape of the Norca API.
func TestAPIStore(t *testing.T) {
	type call struct{ method, path, body string }
	var mu sync.Mutex
	var calls []call
	missing := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, call{r.Method, r.URL.Path, string(raw)})
		notFound := missing
		mu.Unlock()
		if notFound {
			http.Error(w, "object not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	cfg := api.DefaultConfig()
	cfg.Address = srv.URL
	client, err := api.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewAPIStore(client)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if err := store.Apply(ctx, testReg.Object()); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateStatus(ctx, "nginx-edge", map[string]any{"lastHeartbeat": "x"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "nginx-edge"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := append([]call(nil), calls...)
	missing = true
	mu.Unlock()

	want := []call{
		{"PUT", "/v1/object/norca.io/LoadBalancerClass/nginx-edge", ""},
		{"PUT", "/v1/object/norca.io/LoadBalancerClass/nginx-edge/status", ""},
		{"DELETE", "/v1/object/norca.io/LoadBalancerClass/nginx-edge", ""},
	}
	if len(got) != len(want) {
		t.Fatalf("calls: %+v", got)
	}
	for i := range want {
		if got[i].method != want[i].method || got[i].path != want[i].path {
			t.Fatalf("call %d: %s %s, want %s %s", i, got[i].method, got[i].path, want[i].method, want[i].path)
		}
	}

	// A missing object maps to ErrNotFound on a status update, and a delete
	// of a missing object is not an error.
	if err := store.UpdateStatus(ctx, "nginx-edge", map[string]any{}); err == nil || !isNotFound(err) {
		t.Fatalf("status of a missing object: %v", err)
	}
	if err := store.Delete(ctx, "nginx-edge"); err != nil {
		t.Fatalf("delete of a missing object: %v", err)
	}
}

func isNotFound(err error) bool {
	for ; err != nil; err = unwrap(err) {
		if err == ErrNotFound {
			return true
		}
	}
	return false
}

func unwrap(err error) error {
	u, ok := err.(interface{ Unwrap() error })
	if !ok {
		return nil
	}
	return u.Unwrap()
}
