// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package tcpproxy is a provider type that runs the load balancers inside the
// provider process: every load balancer is a TCP listener that forwards each
// connection to the next target, round robin, and tries the next one when a
// target does not answer. It needs no external software, so it is the type to
// try the whole chain, from a service to traffic reaching it, on one machine.
//
// The load balancers live in the process: after a restart they are gone until
// the operator calls the provider again (within a minute for a load balancer
// that is not ready, and when its targets change). The listeners bind on the
// machine the provider runs on; the firewall must allow their ports.
//
// Type ID: "tcpproxy".
package tcpproxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bacanoo-io/norca-loadbalancer/pkg/provider"
)

// TypeID is the ID of the type.
const TypeID = "tcpproxy"

var params = []provider.Param{
	{Name: "advertise_address", Type: provider.ParamString, Required: true,
		Description: "address clients use to reach the load balancers (an address of this machine)"},
	{Name: "bind_address", Type: provider.ParamString, Default: "0.0.0.0",
		Description: "address the load balancers listen on"},
	{Name: "dial_timeout", Type: provider.ParamDuration, Default: 3 * time.Second, Dynamic: true,
		Description: "how long to wait for a target before trying the next one"},
	{Name: "ports", Type: provider.ParamString, Default: "",
		Description: "frontend ports the class supports, such as 8000-9000; empty means any"},
}

func init() {
	provider.Register(provider.Type{
		ID:          TypeID,
		Description: "TCP load balancers inside the provider process; round robin with failover",
		Params:      params,
		New:         New,
	})
}

// Provider is the tcpproxy instance.
type Provider struct {
	advertise string
	bind      string
	caps      provider.Capabilities
	logger    *slog.Logger

	mu  sync.Mutex
	lbs map[provider.Ref]*balancer
	// ports maps a bound port to the load balancer that owns it.
	ports map[int]provider.Ref
}

// New creates an instance.
func New(cfg provider.Config) (provider.Provider, error) {
	for _, name := range []string{"advertise_address", "bind_address"} {
		if ip := cfg.Values.String(name); net.ParseIP(ip) == nil {
			return nil, fmt.Errorf("%s %q is not an IP address", name, ip)
		}
	}
	return &Provider{
		advertise: cfg.Values.String("advertise_address"),
		bind:      cfg.Values.String("bind_address"),
		caps:      provider.Capabilities{Ports: cfg.Values.String("ports")},
		logger:    cfg.Log(),
		lbs:       map[provider.Ref]*balancer{},
		ports:     map[int]provider.Ref{},
	}, nil
}

// Capabilities implements provider.Provider. The listeners are on this
// machine, so there are no internal load balancers.
func (p *Provider) Capabilities() provider.Capabilities { return p.caps }

// Ensure implements provider.Provider.
func (p *Provider) Ensure(ctx context.Context, req provider.Request) (provider.State, error) {
	targets := make([]string, len(req.Targets))
	for i, t := range req.Targets {
		targets[i] = net.JoinHostPort(t.Address, strconv.Itoa(t.Port))
	}
	slices.Sort(targets)
	dial := req.Params.Duration("dial_timeout")

	p.mu.Lock()
	defer p.mu.Unlock()
	if owner, taken := p.ports[req.Port]; taken && owner != req.Ref {
		return provider.State{}, provider.Invalid("port %d is used by load balancer %s", req.Port, owner)
	}

	lb := p.lbs[req.Ref]
	if lb != nil && lb.port != req.Port {
		// The frontend port changed: a new listener replaces the old one.
		p.closeLocked(req.Ref)
		lb = nil
	}
	if lb == nil {
		var lc net.ListenConfig
		ln, err := lc.Listen(ctx, "tcp", net.JoinHostPort(p.bind, strconv.Itoa(req.Port)))
		if err != nil {
			return provider.State{}, fmt.Errorf("listening on %s:%d: %w", p.bind, req.Port, err)
		}
		lb = newBalancer(ln, req.Port, p.logger.With("loadbalancer", req.Ref.String()))
		p.lbs[req.Ref] = lb
		p.ports[req.Port] = req.Ref
		go lb.serve()
	}
	lb.set(targets, dial)

	state := provider.State{
		Addresses: []string{net.JoinHostPort(p.advertise, strconv.Itoa(req.Port))},
		Resource:  "tcpproxy/" + req.Ref.String(),
	}
	if len(targets) == 0 {
		state.Message = "no targets"
	} else {
		state.Ready = true
	}
	return state, nil
}

// Delete implements provider.Provider.
func (p *Provider) Delete(_ context.Context, ref provider.Ref) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closeLocked(ref)
	return nil
}

func (p *Provider) closeLocked(ref provider.Ref) {
	lb := p.lbs[ref]
	if lb == nil {
		return
	}
	lb.close()
	delete(p.lbs, ref)
	delete(p.ports, lb.port)
}

// Close stops every listener.
func (p *Provider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for ref := range p.lbs {
		p.closeLocked(ref)
	}
	return nil
}

// Targets returns the current targets of a load balancer, for tests and
// diagnostics.
func (p *Provider) Targets(ref provider.Ref) []string {
	p.mu.Lock()
	lb := p.lbs[ref]
	p.mu.Unlock()
	if lb == nil {
		return nil
	}
	return lb.snapshot().targets
}

// balancer is one listener.
type balancer struct {
	ln     net.Listener
	port   int
	logger *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc

	state atomic.Pointer[balancerState]
	next  atomic.Uint64

	mu    sync.Mutex
	conns map[net.Conn]struct{}
	done  bool
}

type balancerState struct {
	targets []string
	dial    time.Duration
}

func newBalancer(ln net.Listener, port int, logger *slog.Logger) *balancer {
	ctx, cancel := context.WithCancel(context.Background())
	b := &balancer{
		ln:     ln,
		port:   port,
		logger: logger,
		ctx:    ctx,
		cancel: cancel,
		conns:  map[net.Conn]struct{}{},
	}
	b.state.Store(&balancerState{dial: 3 * time.Second})
	return b
}

func (b *balancer) set(targets []string, dial time.Duration) {
	if dial <= 0 {
		dial = 3 * time.Second
	}
	b.state.Store(&balancerState{targets: targets, dial: dial})
}

func (b *balancer) snapshot() *balancerState { return b.state.Load() }

func (b *balancer) serve() {
	for {
		c, err := b.ln.Accept()
		if err != nil {
			return
		}
		if !b.track(c) {
			_ = c.Close()
			return
		}
		go b.handle(c)
	}
}

func (b *balancer) track(c net.Conn) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.done {
		return false
	}
	b.conns[c] = struct{}{}
	return true
}

func (b *balancer) untrack(c net.Conn) {
	b.mu.Lock()
	delete(b.conns, c)
	b.mu.Unlock()
	_ = c.Close()
}

// handle connects the client to the next target that answers.
func (b *balancer) handle(client net.Conn) {
	defer b.untrack(client)
	st := b.snapshot()
	if len(st.targets) == 0 {
		return
	}
	n := uint64(len(st.targets))
	start := int((b.next.Add(1) - 1) % n) //nolint:gosec // G115: result of modulo len(st.targets) always fits in int
	dialer := net.Dialer{Timeout: st.dial}
	var backend net.Conn
	for i := range st.targets {
		target := st.targets[(start+i)%len(st.targets)]
		c, err := dialer.DialContext(b.ctx, "tcp", target)
		if err != nil {
			b.logger.Debug("target did not answer", "target", target, "error", err)
			continue
		}
		backend = c
		break
	}
	if backend == nil {
		b.logger.Warn("no target answered", "targets", len(st.targets))
		return
	}
	if !b.track(backend) {
		_ = backend.Close()
		return
	}
	defer b.untrack(backend)

	pipe := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		// Pass the end of the stream on, so a request/response protocol sees
		// the half-close; then the other direction can finish.
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}
	var wg sync.WaitGroup
	wg.Go(func() { pipe(backend, client) })
	wg.Go(func() { pipe(client, backend) })
	wg.Wait()
}

func (b *balancer) close() {
	b.cancel()
	b.mu.Lock()
	b.done = true
	conns := make([]net.Conn, 0, len(b.conns))
	for c := range b.conns {
		conns = append(conns, c)
	}
	b.mu.Unlock()
	_ = b.ln.Close()
	for _, c := range conns {
		_ = c.Close()
	}
}
