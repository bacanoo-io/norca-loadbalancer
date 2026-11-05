// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package config reads the configuration file and builds the classes from it.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/bacanoo-io/norca-loadbalancer/pkg/provider"
)

// File is the configuration file (JSON).
type File struct {
	// Listen is the address the provider API listens on (default ":8443").
	Listen string `json:"listen"`

	// TLS makes the API https. Without it the API is http, which the
	// operator only accepts with -class-allow-http.
	TLS *TLS `json:"tls"`

	// AdvertiseURL is the base URL the operator uses to reach this process,
	// for example "https://lb-provider.service.norca:8443". The endpoint of
	// a class is AdvertiseURL + "/classes/<class>".
	AdvertiseURL string `json:"advertise_url"`

	// HeartbeatInterval is how often the status of the classes is updated
	// (default 30s).
	HeartbeatInterval string `json:"heartbeat_interval"`

	// Classes are the classes this process serves.
	Classes []Class `json:"classes"`
}

// ExpandEnv replaces ${NAME} and ${NAME:-default} in every string of the
// configuration with the environment of the process, so a file can be written
// for a job whose ports and addresses are only known at run time, for example
// "listen": ":${NORCA_PORT_api}". A variable that is not set and has no
// default is an error; $NAME without braces is left as it is.
func ExpandEnv(raw []byte, lookup func(string) (string, bool)) ([]byte, error) {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	var firstErr error
	var walk func(v any) any
	walk = func(v any) any {
		switch t := v.(type) {
		case string:
			s, err := expandString(t, lookup)
			if err != nil && firstErr == nil {
				firstErr = err
			}
			return s
		case []any:
			for i := range t {
				t[i] = walk(t[i])
			}
		case map[string]any:
			for k := range t {
				t[k] = walk(t[k])
			}
		}
		return v
	}
	doc = walk(doc)
	if firstErr != nil {
		return nil, firstErr
	}
	return json.Marshal(doc)
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

func expandString(s string, lookup func(string) (string, bool)) (string, error) {
	var err error
	out := envRef.ReplaceAllStringFunc(s, func(m string) string {
		sub := envRef.FindStringSubmatch(m)
		if v, ok := lookup(sub[1]); ok && v != "" {
			return v
		}
		if strings.Contains(m, ":-") {
			return sub[2]
		}
		if err == nil {
			err = fmt.Errorf("environment variable %s is not set (used as %s)", sub[1], m)
		}
		return m
	})
	return out, err
}

// TLS is the certificate of the https API.
type TLS struct {
	Cert string `json:"cert"`
	Key  string `json:"key"`
}

// Class configures one class: an instance of a type.
type Class struct {
	// Class is the class name, which a service names in
	// expose { load_balancer { class = "..." } }.
	Class string `json:"class"`

	// Type is the ID of the implementation.
	Type string `json:"type"`

	// Endpoint is the full URL the operator calls for this class. Without it
	// the endpoint is advertise_url + "/classes/<class>". Set it when the
	// class is reached on another address or port than the rest, for example
	// through a proxy or a port mapping.
	Endpoint string `json:"endpoint"`

	// Token is the bearer token callers must send; "env:NAME" and
	// "file:/path" are resolved. Required unless the class sets no_auth.
	Token string `json:"token"`

	// NoAuth accepts calls without a token. Only for a trusted network.
	NoAuth bool `json:"no_auth"`

	// TokenVariable is the path of the Variable holding the same token on
	// the Norca side, where the operator reads it (item "token").
	TokenVariable string `json:"token_variable"`

	// Params are the parameters of the type.
	Params map[string]any `json:"params"`
}

// Load reads and checks a configuration file.
func Load(path string) (*File, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: reading user-specified config file path by design
	if err != nil {
		return nil, err
	}
	if raw, err = ExpandEnv(raw, os.LookupEnv); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var f File
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := f.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &f, nil
}

// Validate checks what does not need the registry.
func (f *File) Validate() error {
	if f.Listen == "" {
		f.Listen = ":8443"
	}
	if f.AdvertiseURL != "" {
		if err := checkURL(f.AdvertiseURL); err != nil {
			return fmt.Errorf("advertise_url: %w", err)
		}
	}
	if f.TLS != nil && (f.TLS.Cert == "" || f.TLS.Key == "") {
		return fmt.Errorf("tls needs cert and key")
	}
	if f.HeartbeatInterval != "" {
		if _, err := time.ParseDuration(f.HeartbeatInterval); err != nil {
			return fmt.Errorf("heartbeat_interval: %w", err)
		}
	}
	if len(f.Classes) == 0 {
		return fmt.Errorf("at least one class is required")
	}
	seen := map[string]bool{}
	for i, c := range f.Classes {
		if !provider.ValidClass(c.Class) {
			return fmt.Errorf("classes[%d]: invalid class name %q", i, c.Class)
		}
		if seen[c.Class] {
			return fmt.Errorf("classes[%d]: class %q is configured twice", i, c.Class)
		}
		seen[c.Class] = true
		if c.Type == "" {
			return fmt.Errorf("class %q: type is required", c.Class)
		}
		switch {
		case c.Endpoint != "":
			if err := checkURL(c.Endpoint); err != nil {
				return fmt.Errorf("class %q: endpoint: %w", c.Class, err)
			}
		case f.AdvertiseURL == "":
			return fmt.Errorf("class %q: needs an endpoint, or advertise_url for all classes", c.Class)
		}
		if c.Token == "" && !c.NoAuth {
			return fmt.Errorf("class %q: token is required (or no_auth for a trusted network)", c.Class)
		}
	}
	return nil
}

func checkURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("%q must be an http or https URL", s)
	}
	return nil
}

// Interval returns the heartbeat interval.
func (f *File) Interval() time.Duration {
	d, err := time.ParseDuration(f.HeartbeatInterval)
	if err != nil || d <= 0 {
		return 30 * time.Second
	}
	return d
}

// BuiltClass is an instantiated class with its resolved endpoint, token and
// published metadata.
type BuiltClass struct {
	Class         string
	Instance      *provider.Instance
	Token         string
	TokenVariable string
	Endpoint      string
	Internal      bool
	Ports         string
	Parameters    map[string]any
}

// Built is the result of Build.
type Built struct {
	Classes []BuiltClass
}

// Build creates the provider instances with the registry and resolves their
// tokens and endpoints.
func (f *File) Build(reg *provider.Registry) (*Built, error) {
	out := &Built{Classes: make([]BuiltClass, 0, len(f.Classes))}
	base := strings.TrimRight(f.AdvertiseURL, "/")
	for _, c := range f.Classes {
		inst, err := reg.Instantiate(c.Class, c.Type, c.Params)
		if err != nil {
			out.Close()
			return nil, err
		}
		token := ""
		if c.Token != "" {
			if token, err = provider.ResolveSecret(c.Token); err != nil {
				if cl, ok := inst.Provider.(provider.Closer); ok {
					_ = cl.Close()
				}
				out.Close()
				return nil, fmt.Errorf("class %q: token: %w", c.Class, err)
			}
		}
		caps := inst.Provider.Capabilities()
		endpoint := c.Endpoint
		if endpoint == "" {
			endpoint = base + "/classes/" + c.Class
		}
		out.Classes = append(out.Classes, BuiltClass{
			Class:         c.Class,
			Instance:      inst,
			Token:         token,
			TokenVariable: c.TokenVariable,
			Endpoint:      endpoint,
			Internal:      caps.Internal,
			Ports:         caps.Ports,
			Parameters:    inst.Published(),
		})
	}
	return out, nil
}

// Close releases the instances.
func (b *Built) Close() {
	for _, c := range b.Classes {
		if cl, ok := c.Instance.Provider.(provider.Closer); ok {
			_ = cl.Close()
		}
	}
}
