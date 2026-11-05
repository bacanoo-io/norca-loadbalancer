// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package provider defines what a load balancer implementation looks like and
// how implementations register themselves.
//
// An implementation lives in its own package under internal/providers/,
// declares a Type (its ID, its parameters and a constructor) and registers it
// in init(), like a database/sql driver:
//
//	func init() {
//		provider.Register(provider.Type{ID: "nginx", Params: params, New: New})
//	}
//
// The binary links the implementations it wants with blank imports (see
// internal/providers/all). A configuration then creates any number of
// instances of a type, each with its own class name and parameters.
package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Ref identifies a load balancer: the namespace and name of the LoadBalancer
// object of the operator.
type Ref struct {
	Namespace string
	Name      string
}

func (r Ref) String() string { return r.Namespace + "/" + r.Name }

// Target is a healthy instance of the service behind a load balancer.
type Target struct {
	Address string
	Port    int
	AllocID string
}

// Request is what a provider is asked to make true. Ensure is called with
// the complete desired state every time something changes.
type Request struct {
	Ref

	// Service is the name of the service the targets belong to.
	Service string

	// Port is the frontend port.
	Port int

	// Internal asks for a load balancer that is not reachable from the
	// internet. It is only set if the provider declared the capability.
	Internal bool

	// Targets are the healthy instances. The list may be empty.
	Targets []Target

	// Params are the effective parameters of the call: the configured ones,
	// with the dynamic ones overridden by the LoadBalancerClass object.
	Params Values
}

// State is what the provider reports back.
type State struct {
	// Ready is true when the load balancer serves traffic as requested.
	Ready bool

	// Addresses are the frontend addresses ("host:port").
	Addresses []string

	// Resource identifies what was created, for example the cloud ID.
	Resource string

	// Message explains a state that is not ready.
	Message string
}

// Capabilities are what an instance supports. They are published in the
// LoadBalancerClass object and checked by the operator before a call.
type Capabilities struct {
	// Internal is true if the instance can create load balancers that are
	// not reachable from the internet.
	Internal bool

	// Ports limits the frontend ports ("80,443,8000-8100"); empty means any.
	Ports string
}

// Provider is an instance of a type: it creates the load balancers of one
// class. All methods must be safe for concurrent use; calls for the same
// load balancer are serialized by the server.
type Provider interface {
	Capabilities() Capabilities

	// Ensure creates or updates the load balancer so that it forwards to
	// the targets. It must be idempotent. An error wrapping ErrInvalid
	// means the request cannot be satisfied (the operator does not retry it
	// with the same input); any other error is a failure to try again.
	Ensure(ctx context.Context, req Request) (State, error)

	// Delete removes everything created for the load balancer. Deleting
	// what does not exist is not an error.
	Delete(ctx context.Context, ref Ref) error
}

// Closer is implemented by providers that hold resources.
type Closer interface {
	Close() error
}

// ErrInvalid marks a request a provider cannot satisfy.
var ErrInvalid = errors.New("invalid request")

// Invalid returns an error wrapping ErrInvalid.
func Invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Config is the input of the constructor of a type.
type Config struct {
	// Class is the class name of the instance.
	Class string

	// Values are the resolved parameters of the instance.
	Values Values

	// Logger is the logger for the instance; may be nil.
	Logger *slog.Logger
}

// Log returns the configured logger, or slog.Default() if none was set.
func (c Config) Log() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

// Type is an implementation: a kind of load balancer with its own ID.
type Type struct {
	// ID is the identifier of the type, such as "nginx" or "mock".
	ID string

	Description string

	// Params declares the settings of an instance.
	Params []Param

	// New creates an instance.
	New func(Config) (Provider, error)
}

// Instance is a configured provider: a class, served by a type.
type Instance struct {
	Class    string
	Type     Type
	Provider Provider

	// Values are the parameters of the instance as configured.
	Values Values
}

// Effective returns the parameters of one call: the configured values with
// the dynamic ones of the LoadBalancerClass object applied.
func (i *Instance) Effective(raw map[string]any) (Values, error) {
	return Overlay(i.Type.Params, i.Values, raw)
}

// Published returns the parameters written to the LoadBalancerClass object.
func (i *Instance) Published() map[string]any {
	return Published(i.Type.Params, i.Values)
}

var (
	validTypeID = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	validClass  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
)

// ValidClass reports whether name can be a class name.
func ValidClass(name string) bool { return validClass.MatchString(name) }

// Registry holds the known types. Implementations register in Default from
// init(); tests use their own.
type Registry struct {
	mu    sync.RWMutex
	types map[string]Type
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{types: map[string]Type{}} }

// Default is the registry the implementations register in.
var Default = NewRegistry()

// Register adds a type. It panics on a programming error: an invalid or
// duplicate ID, a missing constructor, or invalid parameter declarations.
func (r *Registry) Register(t Type) {
	if !validTypeID.MatchString(t.ID) {
		panic(fmt.Sprintf("provider: invalid type ID %q", t.ID))
	}
	if t.New == nil {
		panic(fmt.Sprintf("provider: type %q has no constructor", t.ID))
	}
	if err := validateParams(t.Params); err != nil {
		panic(fmt.Sprintf("provider: type %q: %v", t.ID, err))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.types[t.ID]; dup {
		panic(fmt.Sprintf("provider: type %q registered twice", t.ID))
	}
	r.types[t.ID] = t
}

// Lookup returns a type by ID.
func (r *Registry) Lookup(id string) (Type, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.types[id]
	return t, ok
}

// Types returns the registered types sorted by ID.
func (r *Registry) Types() []Type {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Type, 0, len(r.types))
	for _, t := range r.types {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Instantiate creates the instance of a class: the type is looked up by ID,
// the parameters are validated and defaulted, and the constructor runs.
func (r *Registry) Instantiate(class, typeID string, raw map[string]any) (*Instance, error) {
	if !ValidClass(class) {
		return nil, fmt.Errorf("invalid class name %q", class)
	}
	t, ok := r.Lookup(typeID)
	if !ok {
		return nil, fmt.Errorf("class %q: unknown type %q (linked types: %s)", class, typeID, r.names())
	}
	values, err := Resolve(t.Params, raw)
	if err != nil {
		return nil, fmt.Errorf("class %q (type %s): %w", class, typeID, err)
	}
	p, err := t.New(Config{Class: class, Values: values, Logger: slog.Default()})
	if err != nil {
		return nil, fmt.Errorf("class %q (type %s): %w", class, typeID, err)
	}
	return &Instance{Class: class, Type: t, Provider: p, Values: values}, nil
}

func (r *Registry) names() string {
	types := r.Types()
	if len(types) == 0 {
		return "none"
	}
	var b strings.Builder
	for i, t := range types {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(t.ID)
	}
	return b.String()
}

// Register adds a type to the Default registry. Implementations call it from
// init().
func Register(t Type) { Default.Register(t) }
