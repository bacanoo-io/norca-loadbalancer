// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package server implements the provider API the LoadBalancer operator of
// Norca calls, and serves any number of classes from one process:
//
//	PUT    /classes/{class}/v1/loadbalancers/{namespace}/{name}   create or update
//	DELETE /classes/{class}/v1/loadbalancers/{namespace}/{name}   remove
//
// The endpoint a class registers is the base URL of the server plus
// /classes/{class}. Two read-only endpoints describe what the process
// offers:
//
//	GET /v1/types     the linked types with their parameters
//	GET /v1/classes   the configured classes
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bacanoo-io/norca-loadbalancer/pkg/provider"
)

// Class is a served class: an instance and the token its callers must send.
type Class struct {
	Instance *provider.Instance

	// Token is the bearer token of the calls. Empty disables the check; use
	// that only on a trusted network.
	Token string
}

// Server serves the classes.
type Server struct {
	classes map[string]*Class
	types   []provider.Type
	logger  *slog.Logger
	locks   keyedLocks

	// Timeout bounds one call of a provider.
	Timeout time.Duration
}

// New returns a server for the classes. The types are listed on /v1/types.
func New(classes []*Class, types []provider.Type, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &Server{classes: make(map[string]*Class, len(classes)), types: types, logger: logger, Timeout: 2 * time.Minute}
	for _, c := range classes {
		s.classes[c.Instance.Class] = c
	}
	return s
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /v1/types", s.listTypes)
	mux.HandleFunc("GET /v1/classes", s.listClasses)
	mux.HandleFunc("PUT /classes/{class}/v1/loadbalancers/{namespace}/{name}", s.put)
	mux.HandleFunc("DELETE /classes/{class}/v1/loadbalancers/{namespace}/{name}", s.delete)
	return mux
}

// Request and response bodies of the provider API.
type (
	putRequest struct {
		Name       string         `json:"name"`
		Namespace  string         `json:"namespace"`
		Service    string         `json:"service"`
		Port       int            `json:"port"`
		Internal   bool           `json:"internal"`
		Targets    []putTarget    `json:"targets"`
		Parameters map[string]any `json:"parameters"`
	}
	putTarget struct {
		Address string `json:"address"`
		Port    int    `json:"port"`
		AllocID string `json:"allocID"`
	}
	putResponse struct {
		Ready     bool     `json:"ready"`
		Addresses []string `json:"addresses"`
		Resource  string   `json:"resource"`
		Message   string   `json:"message"`
	}
)

// validName matches the namespace and name of an object; it keeps them safe
// to use in file names and cloud resource names.
var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func (s *Server) class(w http.ResponseWriter, r *http.Request) (*Class, provider.Ref, bool) {
	c, ok := s.classes[r.PathValue("class")]
	if !ok {
		httpError(w, http.StatusNotFound, "unknown class")
		return nil, provider.Ref{}, false
	}
	if c.Token != "" && !validToken(r, c.Token) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		httpError(w, http.StatusUnauthorized, "invalid or missing token")
		return nil, provider.Ref{}, false
	}
	ref := provider.Ref{Namespace: r.PathValue("namespace"), Name: r.PathValue("name")}
	if !validName.MatchString(ref.Namespace) || !validName.MatchString(ref.Name) ||
		strings.Contains(ref.Namespace, "..") || strings.Contains(ref.Name, "..") {
		httpError(w, http.StatusBadRequest, "invalid namespace or name")
		return nil, provider.Ref{}, false
	}
	return c, ref, true
}

func validToken(r *http.Request, token string) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

func (s *Server) put(w http.ResponseWriter, r *http.Request) {
	c, ref, ok := s.class(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body putRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if (body.Name != "" && body.Name != ref.Name) || (body.Namespace != "" && body.Namespace != ref.Namespace) {
		httpError(w, http.StatusBadRequest, "name and namespace of the body differ from the path")
		return
	}
	req, err := s.buildRequest(c, ref, body)
	if err != nil {
		httpError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.Timeout)
	defer cancel()
	unlock := s.locks.lock(c.Instance.Class + "/" + ref.String())
	defer unlock()

	state, err := c.Instance.Provider.Ensure(ctx, req)
	if err != nil {
		s.logger.Warn("ensure failed", "class", c.Instance.Class, "loadbalancer", ref.String(), "error", err)
		status := http.StatusBadGateway
		if errors.Is(err, provider.ErrInvalid) {
			status = http.StatusUnprocessableEntity
		}
		httpError(w, status, err.Error())
		return
	}
	// host is the address the operator used (the registered endpoint), local
	// the address the request arrived on; they differ behind a port mapping
	// or a proxy.
	s.logger.Info("ensured", "class", c.Instance.Class, "loadbalancer", ref.String(),
		"targets", len(req.Targets), "ready", state.Ready, "host", r.Host, "local", localAddr(r))
	writeJSON(w, http.StatusOK, putResponse{
		Ready: state.Ready, Addresses: nonNil(state.Addresses), Resource: state.Resource, Message: state.Message,
	})
}

// buildRequest validates the body and applies the dynamic parameters.
func (s *Server) buildRequest(c *Class, ref provider.Ref, body putRequest) (provider.Request, error) {
	if body.Port < 1 || body.Port > 65535 {
		return provider.Request{}, fmt.Errorf("port %d is out of range", body.Port)
	}
	caps := c.Instance.Provider.Capabilities()
	if body.Internal && !caps.Internal {
		return provider.Request{}, fmt.Errorf("class %q cannot create internal load balancers", c.Instance.Class)
	}
	targets := make([]provider.Target, len(body.Targets))
	for i, t := range body.Targets {
		if t.Address == "" || t.Port < 1 || t.Port > 65535 {
			return provider.Request{}, fmt.Errorf("invalid target %d: %q port %d", i, t.Address, t.Port)
		}
		targets[i] = provider.Target{Address: t.Address, Port: t.Port, AllocID: t.AllocID}
	}
	values, err := c.Instance.Effective(body.Parameters)
	if err != nil {
		return provider.Request{}, err
	}
	return provider.Request{
		Ref: ref, Service: body.Service, Port: body.Port, Internal: body.Internal,
		Targets: targets, Params: values,
	}, nil
}

func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
	c, ref, ok := s.class(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.Timeout)
	defer cancel()
	unlock := s.locks.lock(c.Instance.Class + "/" + ref.String())
	defer unlock()

	if err := c.Instance.Provider.Delete(ctx, ref); err != nil {
		s.logger.Warn("delete failed", "class", c.Instance.Class, "loadbalancer", ref.String(), "error", err)
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.logger.Info("deleted", "class", c.Instance.Class, "loadbalancer", ref.String(), "host", r.Host, "local", localAddr(r))
	w.WriteHeader(http.StatusNoContent)
}

// typeInfo describes a type on /v1/types.
type typeInfo struct {
	ID          string      `json:"id"`
	Description string      `json:"description"`
	Parameters  []paramInfo `json:"parameters"`
}

type paramInfo struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Default     any      `json:"default,omitempty"`
	Secret      bool     `json:"secret,omitempty"`
	Dynamic     bool     `json:"dynamic,omitempty"`
	Enum        []string `json:"enum,omitempty"`
}

func (s *Server) listTypes(w http.ResponseWriter, _ *http.Request) {
	out := make([]typeInfo, 0, len(s.types))
	for _, t := range s.types {
		info := typeInfo{ID: t.ID, Description: t.Description, Parameters: []paramInfo{}}
		for _, p := range t.Params {
			def := p.Default
			if d, ok := def.(time.Duration); ok {
				def = d.String()
			}
			info.Parameters = append(info.Parameters, paramInfo{
				Name: p.Name, Type: string(p.Type), Description: p.Description, Required: p.Required,
				Default: def, Secret: p.Secret, Dynamic: p.Dynamic, Enum: p.Enum,
			})
		}
		out = append(out, info)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listClasses(w http.ResponseWriter, _ *http.Request) {
	type classInfo struct {
		Class        string         `json:"class"`
		Type         string         `json:"type"`
		Internal     bool           `json:"internal"`
		Ports        string         `json:"ports,omitempty"`
		Parameters   map[string]any `json:"parameters"`
		TokenChecked bool           `json:"tokenChecked"`
	}
	out := make([]classInfo, 0, len(s.classes))
	for _, c := range s.classes {
		caps := c.Instance.Provider.Capabilities()
		out = append(out, classInfo{
			Class: c.Instance.Class, Type: c.Instance.Type.ID, Internal: caps.Internal, Ports: caps.Ports,
			Parameters: c.Instance.Published(), TokenChecked: c.Token != "",
		})
	}
	slices.SortFunc(out, func(a, b classInfo) int { return strings.Compare(a.Class, b.Class) })
	writeJSON(w, http.StatusOK, out)
}

// localAddr returns the local address the connection of the request arrived
// on.
func localAddr(r *http.Request) string {
	if a, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		return a.String()
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// keyedLocks serializes the calls for one load balancer.
type keyedLocks struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

type keyedLock struct {
	mu   sync.Mutex
	refs int
}

func (k *keyedLocks) lock(key string) (unlock func()) {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*keyedLock{}
	}
	l := k.locks[key]
	if l == nil {
		l = &keyedLock{}
		k.locks[key] = l
	}
	l.refs++
	k.mu.Unlock()

	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		k.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}
