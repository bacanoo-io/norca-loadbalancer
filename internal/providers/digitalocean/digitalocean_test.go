// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package digitalocean

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bacanoo-io/norca-loadbalancer/pkg/provider"
)

type fakeDOServer struct {
	mu       sync.Mutex
	token    string
	lbs      map[string]*doLoadBalancer
	droplets []doDroplet
}

func newFakeDO(t *testing.T, token string) (*fakeDOServer, *httptest.Server) {
	t.Helper()
	d1 := doDroplet{ID: 1001}
	d1.Networks.V4 = append(d1.Networks.V4, struct {
		IPAddress string `json:"ip_address"`
		Type      string `json:"type"`
	}{IPAddress: "10.10.0.1", Type: "private"})
	d2 := doDroplet{ID: 1002}
	d2.Networks.V4 = append(d2.Networks.V4, struct {
		IPAddress string `json:"ip_address"`
		Type      string `json:"type"`
	}{IPAddress: "10.10.0.2", Type: "private"})
	d3 := doDroplet{ID: 1003}
	d3.Networks.V4 = append(d3.Networks.V4, struct {
		IPAddress string `json:"ip_address"`
		Type      string `json:"type"`
	}{IPAddress: "10.10.0.3", Type: "private"})

	f := &fakeDOServer{
		token:    token,
		lbs:      map[string]*doLoadBalancer{},
		droplets: []doDroplet{d1, d2, d3},
	}
	srv := httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeDOServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(doErrorResponse{ID: "unauthorized", Message: "invalid token"})
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/droplets":
		_ = json.NewEncoder(w).Encode(doListDropletsResponse{Droplets: f.droplets})

	case r.Method == http.MethodGet && r.URL.Path == "/load_balancers":
		var list []doLoadBalancer
		for _, lb := range f.lbs {
			list = append(list, *lb)
		}
		_ = json.NewEncoder(w).Encode(doListLBsResponse{LoadBalancers: list})

	case r.Method == http.MethodPost && r.URL.Path == "/load_balancers":
		var lb doLoadBalancer
		_ = json.NewDecoder(r.Body).Decode(&lb)
		lb.ID = "do-lb-1"
		lb.Status = "active"
		if lb.Type == "REGIONAL_NETWORK" {
			lb.IP = "10.10.0.100"
		} else {
			lb.IP = "167.99.0.10"
		}
		f.lbs[lb.ID] = &lb
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(doSingleLBResponse{LoadBalancer: lb})

	case strings.HasPrefix(r.URL.Path, "/load_balancers/"):
		id := strings.TrimPrefix(r.URL.Path, "/load_balancers/")
		existing, ok := f.lbs[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodPut:
			var lb doLoadBalancer
			_ = json.NewDecoder(r.Body).Decode(&lb)
			lb.ID = id
			lb.Status = existing.Status
			lb.IP = existing.IP
			f.lbs[id] = &lb
			_ = json.NewEncoder(w).Encode(doSingleLBResponse{LoadBalancer: lb})
		case http.MethodDelete:
			delete(f.lbs, id)
			w.WriteHeader(http.StatusNoContent)
		}

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestDigitalOceanLifecycle(t *testing.T) {
	fake, srv := newFakeDO(t, "do-secret-token")

	inst, err := provider.Default.Instantiate("do-eu", TypeID, map[string]any{
		"token":    "do-secret-token",
		"region":   "fra1",
		"vpc_uuid": "vpc-uuid-1",
		"endpoint": srv.URL,
	})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	if !inst.Provider.Capabilities().Internal {
		t.Fatalf("expected Internal capability when vpc_uuid is configured")
	}

	ctx := context.Background()
	ref := provider.Ref{Namespace: "prod", Name: "web"}

	// 1. Create LB with 2 target Droplets.
	params1, _ := inst.Effective(nil)
	state, err := inst.Provider.Ensure(ctx, provider.Request{
		Ref:     ref,
		Service: "web",
		Port:    80,
		Targets: []provider.Target{
			{Address: "10.10.0.1", Port: 8080},
			{Address: "10.10.0.2", Port: 8080},
		},
		Params: params1,
	})
	if err != nil {
		t.Fatalf("Ensure create: %v", err)
	}
	if !state.Ready || state.Resource != "digitalocean/lb/do-lb-1" {
		t.Fatalf("unexpected state: %+v", state)
	}
	if !slices.Equal(state.Addresses, []string{"167.99.0.10:80"}) {
		t.Fatalf("unexpected addresses: %v", state.Addresses)
	}

	// 2. Update: dynamic size_unit, algorithm, enable_proxy_protocol, port, and droplet set.
	params2, err := inst.Effective(map[string]any{
		"size_unit":             2,
		"algorithm":             "least_connections",
		"enable_proxy_protocol": true,
	})
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}
	state, err = inst.Provider.Ensure(ctx, provider.Request{
		Ref:     ref,
		Service: "web",
		Port:    443,
		Targets: []provider.Target{
			{Address: "10.10.0.2", Port: 8443},
			{Address: "10.10.0.3", Port: 8443},
		},
		Params: params2,
	})
	if err != nil {
		t.Fatalf("Ensure update: %v", err)
	}
	if !state.Ready || !slices.Equal(state.Addresses, []string{"167.99.0.10:443"}) {
		t.Fatalf("unexpected updated state: %+v", state)
	}

	fake.mu.Lock()
	lb := fake.lbs["do-lb-1"]
	if lb.SizeUnit != 2 || lb.Algorithm != "least_connections" || !lb.EnableProxyProtocol {
		t.Fatalf("unexpected LB settings: %+v", lb)
	}
	if !slices.Equal(lb.DropletIDs, []int{1002, 1003}) {
		t.Fatalf("unexpected droplet IDs: %v", lb.DropletIDs)
	}
	if len(lb.ForwardingRules) != 1 || lb.ForwardingRules[0].EntryPort != 443 || lb.ForwardingRules[0].TargetPort != 8443 {
		t.Fatalf("unexpected forwarding rules: %+v", lb.ForwardingRules)
	}
	fake.mu.Unlock()

	// 3. Delete is idempotent.
	if err := inst.Provider.Delete(ctx, ref); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := inst.Provider.Delete(ctx, ref); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
}

