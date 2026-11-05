// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bacanoo-io/norca-loadbalancer/pkg/provider"
)

func TestPrintTypes(t *testing.T) {
	var buf bytes.Buffer
	printTypes(&buf, provider.Default)
	out := buf.String()
	for _, want := range []string{"mock -", "nginx -", "tcpproxy -", "config_dir", "advertise_address"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestRunAndGracefulDeregister(t *testing.T) {
	var applied, deleted atomic.Int32
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			applied.Add(1)
		case http.MethodDelete:
			deleted.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer apiSrv.Close()

	t.Setenv("NORCA_ADDR", apiSrv.URL)
	cfgPath := filepath.Join(t.TempDir(), "classes.json")
	cfgJSON := `{
		"listen": "127.0.0.1:0",
		"advertise_url": "https://lb.example.com:8443",
		"heartbeat_interval": "50ms",
		"classes": [
			{"class": "mock-edge", "type": "mock", "no_auth": true}
		]
	}`
	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"-config", cfgPath, "-log-level", "error"})
	}()

	deadline := time.Now().Add(5 * time.Second)
	for applied.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("timeout waiting for class registration")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run returned error: %v", err)
	}
	if deleted.Load() != 1 {
		t.Fatalf("expected registration to be deleted before run returned, got %d deletes", deleted.Load())
	}
}
