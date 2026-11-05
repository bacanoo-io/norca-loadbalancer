// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package mock is a provider type that creates no load balancer: it keeps
// them in memory. It is for trying the operator and the provider API without
// a cloud, and a model of how a type is written.
//
// Type ID: "mock".
package mock

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/bacanoo-io/norca-loadbalancer/pkg/provider"
)

// TypeID is the ID of the type.
const TypeID = "mock"

var params = []provider.Param{
	{Name: "address", Type: provider.ParamString, Default: "192.0.2.10",
		Description: "address reported as the frontend of every load balancer"},
	{Name: "ready", Type: provider.ParamBool, Default: true, Dynamic: true,
		Description: "whether the load balancers report ready; change it in the LoadBalancerClass object to simulate an outage"},
	{Name: "latency", Type: provider.ParamDuration, Default: time.Duration(0), Dynamic: true,
		Description: "time every call takes (simulates a slow cloud API)"},
	{Name: "internal", Type: provider.ParamBool, Default: true,
		Description: "whether the class supports internal load balancers"},
	{Name: "ports", Type: provider.ParamString, Default: "",
		Description: "frontend ports the class supports, such as 80,443,8000-8100; empty means any"},
}

func init() {
	provider.Register(provider.Type{
		ID:          TypeID,
		Description: "keeps load balancers in memory; for trials and tests",
		Params:      params,
		New:         New,
	})
}

// LoadBalancer is a load balancer of the mock.
type LoadBalancer struct {
	Ref      provider.Ref
	Port     int
	Internal bool
	Targets  []string
}

// Provider is the mock instance.
type Provider struct {
	class string
	caps  provider.Capabilities
	addr  string

	mu  sync.Mutex
	lbs map[provider.Ref]LoadBalancer
}

// New creates an instance.
func New(cfg provider.Config) (provider.Provider, error) {
	if ip := cfg.Values.String("address"); net.ParseIP(ip) == nil {
		return nil, fmt.Errorf("address %q is not an IP address", ip)
	}
	return &Provider{
		class: cfg.Class,
		addr:  cfg.Values.String("address"),
		caps:  provider.Capabilities{Internal: cfg.Values.Bool("internal"), Ports: cfg.Values.String("ports")},
		lbs:   map[provider.Ref]LoadBalancer{},
	}, nil
}

// Capabilities implements provider.Provider.
func (p *Provider) Capabilities() provider.Capabilities { return p.caps }

// Ensure implements provider.Provider.
func (p *Provider) Ensure(ctx context.Context, req provider.Request) (provider.State, error) {
	if d := req.Params.Duration("latency"); d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return provider.State{}, ctx.Err()
		}
	}
	targets := make([]string, len(req.Targets))
	for i, t := range req.Targets {
		targets[i] = net.JoinHostPort(t.Address, strconv.Itoa(t.Port))
	}
	sort.Strings(targets)

	p.mu.Lock()
	p.lbs[req.Ref] = LoadBalancer{Ref: req.Ref, Port: req.Port, Internal: req.Internal, Targets: targets}
	p.mu.Unlock()

	state := provider.State{
		Addresses: []string{net.JoinHostPort(p.addr, strconv.Itoa(req.Port))},
		Resource:  fmt.Sprintf("mock/%s/%s/%s", p.class, req.Namespace, req.Name),
	}
	switch {
	case !req.Params.Bool("ready"):
		state.Message = "the mock is set to not ready"
	case len(targets) == 0:
		state.Message = "no targets"
	default:
		state.Ready = true
	}
	return state, nil
}

// Delete implements provider.Provider.
func (p *Provider) Delete(_ context.Context, ref provider.Ref) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.lbs, ref)
	return nil
}

// Snapshot returns the load balancers, sorted by reference.
func (p *Provider) Snapshot() []LoadBalancer {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]LoadBalancer, 0, len(p.lbs))
	for _, lb := range p.lbs {
		out = append(out, lb)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref.String() < out[j].Ref.String() })
	return out
}
