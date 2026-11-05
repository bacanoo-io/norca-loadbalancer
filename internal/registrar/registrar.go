// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package registrar writes the LoadBalancerClass objects of the classes a
// process serves and keeps their heartbeat, so the LoadBalancer operator
// knows which classes exist, where to call them and what they support.
package registrar

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/bacanoo-io/norca/norca/api"
)

// The object kind of the registrations (see loadbalancerclass.kind.hcl of
// the operator).
const (
	Group = "norca.io"
	Kind  = "LoadBalancerClass"

	// ManagedBy marks the objects this program owns.
	ManagedBy = "norca-loadbalancer"
)

// ErrNotFound is returned by a Store when the object does not exist.
var ErrNotFound = errors.New("object not found")

// Store is the part of the Norca API the registrar needs.
type Store interface {
	// Apply creates or updates the object (spec and metadata).
	Apply(ctx context.Context, obj *api.CustomObject) error

	// UpdateStatus replaces the status of the object; it returns ErrNotFound
	// if the object does not exist.
	UpdateStatus(ctx context.Context, name string, status map[string]any) error

	// Delete removes the object. Removing a missing object is not an error.
	Delete(ctx context.Context, name string) error
}

// Registration is the LoadBalancerClass of one served class.
type Registration struct {
	Class string

	// Endpoint is the base URL the operator calls for the class.
	Endpoint string

	// TokenVariable is the path of the Variable with the token the operator
	// sends; empty if the class has no token.
	TokenVariable string

	Internal bool
	Ports    string

	// Parameters are the published parameters of the instance.
	Parameters map[string]any
}

// Object returns the LoadBalancerClass object of the registration.
func (r Registration) Object() *api.CustomObject {
	caps := map[string]any{"internal": r.Internal}
	if r.Ports != "" {
		caps["ports"] = r.Ports
	}
	spec := map[string]any{
		"endpoint":     r.Endpoint,
		"capabilities": caps,
	}
	if r.TokenVariable != "" {
		spec["tokenVariable"] = r.TokenVariable
	}
	if len(r.Parameters) > 0 {
		spec["parameters"] = r.Parameters
	}
	return &api.CustomObject{
		Group:     Group,
		Kind:      Kind,
		Name:      r.Class,
		ManagedBy: ManagedBy,
		Spec:      spec,
	}
}

// Registrar registers classes and reports that they are alive.
type Registrar struct {
	Store         Store
	Registrations []Registration

	// Interval is the period of the heartbeat. It should be well below the
	// -class-ttl of the operator (three minutes by default).
	Interval time.Duration

	// Version is reported in the status.
	Version string

	// DeregisterOnExit removes the registrations when Run returns, so the
	// operator reports UnknownClass instead of waiting for the heartbeat to
	// expire.
	DeregisterOnExit bool

	Logger *slog.Logger

	// Now and Retry are replaced in tests.
	Now   func() time.Time
	Retry time.Duration
}

func (r *Registrar) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (r *Registrar) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Run registers the classes (retrying until it succeeds or ctx ends) and
// then reports the heartbeat until ctx ends.
func (r *Registrar) Run(ctx context.Context) error {
	interval := r.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	retry := r.Retry
	if retry <= 0 {
		retry = 5 * time.Second
	}

	for _, reg := range r.Registrations {
		for {
			err := r.register(ctx, reg)
			if err == nil {
				break
			}
			r.logger().Warn("registering the class failed, retrying", "class", reg.Class, "error", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(retry):
			}
		}
		r.logger().Info("class registered", "class", reg.Class, "endpoint", reg.Endpoint)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			if r.DeregisterOnExit {
				r.deregister()
			}
			return nil
		case <-ticker.C:
			for _, reg := range r.Registrations {
				if err := r.heartbeat(ctx, reg); err != nil {
					r.logger().Warn("heartbeat failed", "class", reg.Class, "error", err)
				}
			}
		}
	}
}

func (r *Registrar) status() map[string]any {
	status := map[string]any{"lastHeartbeat": r.now().UTC().Format(time.RFC3339)}
	if r.Version != "" {
		status["version"] = r.Version
	}
	return status
}

func (r *Registrar) register(ctx context.Context, reg Registration) error {
	if err := r.Store.Apply(ctx, reg.Object()); err != nil {
		return err
	}
	return r.Store.UpdateStatus(ctx, reg.Class, r.status())
}

// heartbeat updates the status; if the object was removed meanwhile, it is
// registered again.
func (r *Registrar) heartbeat(ctx context.Context, reg Registration) error {
	err := r.Store.UpdateStatus(ctx, reg.Class, r.status())
	if errors.Is(err, ErrNotFound) {
		r.logger().Warn("the registration disappeared, registering again", "class", reg.Class)
		return r.register(ctx, reg)
	}
	return err
}

func (r *Registrar) deregister() {
	// ctx is done at this point; use a fresh one.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, reg := range r.Registrations {
		if err := r.Store.Delete(ctx, reg.Class); err != nil {
			r.logger().Warn("removing the registration failed", "class", reg.Class, "error", err)
		}
	}
}

// APIStore is a Store on the Norca API.
type APIStore struct {
	Client *api.Client
}

// NewAPIStore returns a Store using client; with nil it uses the
// environment (NORCA_ADDR, NORCA_TOKEN, ...). A provider that runs as a Norca
// job reaches the API on the task API socket with its workload identity:
// NORCA_ADDR=unix://${NORCA_SECRETS_DIR}/api.sock and, with identity { env =
// true }, NORCA_TOKEN. The unix:// address is supported here.
func NewAPIStore(client *api.Client) (*APIStore, error) {
	if client == nil {
		conf := api.DefaultConfig()
		if err := configureUnixSocket(conf); err != nil {
			return nil, err
		}
		var err error
		if client, err = api.NewClient(conf); err != nil {
			return nil, err
		}
	}
	return &APIStore{Client: client}, nil
}

// configureUnixSocket makes the client dial the socket of a unix:// address.
func configureUnixSocket(conf *api.Config) error {
	socket, ok := strings.CutPrefix(conf.Address, "unix://")
	if !ok {
		return nil
	}
	if socket == "" {
		return errors.New("the unix socket address has no path")
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	conf.Address = "http://127.0.0.1"
	conf.HttpClient = &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", socket)
			},
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     90 * time.Second,
		},
	}
	return nil
}

// Apply implements Store.
func (s *APIStore) Apply(ctx context.Context, obj *api.CustomObject) error {
	w := (&api.WriteOptions{}).WithContext(ctx)
	_, _, err := s.Client.Objects().Apply(obj, nil, w)
	return wrap(err)
}

// UpdateStatus implements Store.
func (s *APIStore) UpdateStatus(ctx context.Context, name string, status map[string]any) error {
	w := (&api.WriteOptions{}).WithContext(ctx)
	_, _, err := s.Client.Objects().UpdateStatus(Group, Kind, name, status, nil, w)
	return wrap(err)
}

// Delete implements Store.
func (s *APIStore) Delete(ctx context.Context, name string) error {
	w := (&api.WriteOptions{}).WithContext(ctx)
	_, _, err := s.Client.Objects().Delete(Group, Kind, name, nil, w)
	if err = wrap(err); errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func wrap(err error) error {
	if err == nil {
		return nil
	}
	var respErr api.UnexpectedResponseError
	if errors.As(err, &respErr) && respErr.StatusCode() == http.StatusNotFound {
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	return err
}
