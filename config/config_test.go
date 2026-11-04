// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bacanoo-io/norca-loadbalancer/config"
	"github.com/bacanoo-io/norca-loadbalancer/provider"
	_ "github.com/bacanoo-io/norca-loadbalancer/providers/all"
)

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "classes.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAndBuild(t *testing.T) {
	t.Setenv("LB_EDGE_TOKEN", "edge-token")
	dir := t.TempDir()
	cfg, err := config.Load(write(t, `{
	  "listen": ":9443",
	  "advertise_url": "https://lb-provider.service.norca:9443/",
	  "heartbeat_interval": "15s",
	  "classes": [
	    {"class": "nginx-edge", "type": "nginx", "token": "env:LB_EDGE_TOKEN",
	     "token_variable": "lb-provider/nginx-edge/token",
	     "params": {"config_dir": "`+strings.ReplaceAll(dir, `\`, `/`)+`", "advertise_address": "203.0.113.5", "ports": "1024-65535"}},
	    {"class": "mock", "type": "mock", "no_auth": true, "params": {"ready": false}}
	  ]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":9443" || cfg.Interval() != 15*time.Second {
		t.Fatalf("listen %q, interval %v", cfg.Listen, cfg.Interval())
	}

	built, err := cfg.Build(provider.Default)
	if err != nil {
		t.Fatal(err)
	}
	defer built.Close()
	if len(built.Classes) != 2 || len(built.Registrations) != 2 {
		t.Fatalf("built %d classes, %d registrations", len(built.Classes), len(built.Registrations))
	}
	if built.Classes[0].Token != "edge-token" || built.Classes[1].Token != "" {
		t.Fatalf("tokens: %q, %q", built.Classes[0].Token, built.Classes[1].Token)
	}

	reg := built.Registrations[0]
	if reg.Endpoint != "https://lb-provider.service.norca:9443/classes/nginx-edge" {
		t.Fatalf("endpoint %q", reg.Endpoint)
	}
	if reg.TokenVariable != "lb-provider/nginx-edge/token" || reg.Ports != "1024-65535" || reg.Internal {
		t.Fatalf("registration: %+v", reg)
	}
	// What is published is what the operator sends back with every call.
	if reg.Parameters["advertise_address"] != "203.0.113.5" {
		t.Fatalf("published parameters: %v", reg.Parameters)
	}
	if _, leaked := reg.Parameters["token"]; leaked {
		t.Fatal("a secret was published")
	}
	if built.Registrations[1].Parameters["ready"] != false {
		t.Fatalf("mock parameters: %v", built.Registrations[1].Parameters)
	}
}

func TestLoadErrors(t *testing.T) {
	cases := map[string]struct{ content, want string }{
		"unknown field": {`{"advertise_url":"https://x","bogus":1,"classes":[]}`, "bogus"},
		"no advertise":  {`{"classes":[{"class":"a","type":"mock","no_auth":true}]}`, "needs an endpoint, or advertise_url"},
		"no classes":    {`{"advertise_url":"https://x"}`, "at least one class"},
		"duplicate": {`{"advertise_url":"https://x","classes":[
			{"class":"a","type":"mock","no_auth":true},{"class":"a","type":"mock","no_auth":true}]}`, "configured twice"},
		"no token":     {`{"advertise_url":"https://x","classes":[{"class":"a","type":"mock"}]}`, "token is required"},
		"bad class":    {`{"advertise_url":"https://x","classes":[{"class":"a b","type":"mock","no_auth":true}]}`, "invalid class name"},
		"no type":      {`{"advertise_url":"https://x","classes":[{"class":"a","no_auth":true}]}`, "type is required"},
		"bad interval": {`{"advertise_url":"https://x","heartbeat_interval":"soon","classes":[{"class":"a","type":"mock","no_auth":true}]}`, "heartbeat_interval"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := config.Load(write(t, tc.content))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
		})
	}
}

func TestBuildErrors(t *testing.T) {
	load := func(class string) *config.File {
		cfg, err := config.Load(write(t, `{"advertise_url":"https://x","classes":[`+class+`]}`))
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	cases := map[string]struct{ class, want string }{
		"unknown type":    {`{"class":"a","type":"hcloud","no_auth":true}`, `unknown type "hcloud" (linked types: mock, nginx, tcpproxy)`},
		"missing param":   {`{"class":"a","type":"nginx","no_auth":true,"params":{"config_dir":"x"}}`, `"advertise_address" is required`},
		"unknown param":   {`{"class":"a","type":"mock","no_auth":true,"params":{"colour":"red"}}`, "unknown parameters: colour"},
		"bad value":       {`{"class":"a","type":"mock","no_auth":true,"params":{"ready":"perhaps"}}`, "must be true or false"},
		"unset token env": {`{"class":"a","type":"mock","token":"env:LB_CFG_UNSET"}`, "LB_CFG_UNSET"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := load(tc.class).Build(provider.Default)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
		})
	}
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("NORCA_PORT_api", "23456")
	t.Setenv("NORCA_IP_api", "10.77.1.12")
	lookup := os.LookupEnv

	got, err := config.ExpandEnv([]byte(`{"listen":":${NORCA_PORT_api}","list":["a","${NORCA_IP_api}"],"n":5,
	  "nested":{"url":"http://${NORCA_IP_api}:${NORCA_PORT_api}","def":"${LB_UNSET_X:-fallback}","bare":"$NORCA_PORT_api"}}`), lookup)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	for _, want := range []string{`":23456"`, `"10.77.1.12"`, `"http://10.77.1.12:23456"`, `"fallback"`, `"$NORCA_PORT_api"`, `"n":5`} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %s in %s", want, s)
		}
	}

	if _, err := config.ExpandEnv([]byte(`{"listen":":${LB_UNSET_Y}"}`), lookup); err == nil ||
		!strings.Contains(err.Error(), "LB_UNSET_Y") {
		t.Fatalf("an unset variable: %v", err)
	}
}

// TestDynamicPorts: a job gets its port when it is placed; the file refers to
// it, and the endpoint a class registers is not the address it listens on.
func TestDynamicPortsAndClassEndpoint(t *testing.T) {
	t.Setenv("NORCA_PORT_api", "23456")
	t.Setenv("NORCA_IP_api", "10.77.1.12")
	cfg, err := config.Load(write(t, `{
	  "listen": "0.0.0.0:${NORCA_PORT_api}",
	  "advertise_url": "http://${NORCA_IP_api}:18443",
	  "classes": [
	    {"class": "mapped", "type": "mock", "no_auth": true},
	    {"class": "elsewhere", "type": "mock", "no_auth": true, "endpoint": "https://lb.example.com/providers/mock"}
	  ]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "0.0.0.0:23456" {
		t.Fatalf("listen %q", cfg.Listen)
	}
	built, err := cfg.Build(provider.Default)
	if err != nil {
		t.Fatal(err)
	}
	if got := built.Registrations[0].Endpoint; got != "http://10.77.1.12:18443/classes/mapped" {
		t.Fatalf("advertised endpoint %q", got)
	}
	if got := built.Registrations[1].Endpoint; got != "https://lb.example.com/providers/mock" {
		t.Fatalf("class endpoint %q", got)
	}

	// Without advertise_url every class needs its own endpoint.
	_, err = config.Load(write(t, `{"classes":[{"class":"a","type":"mock","no_auth":true}]}`))
	if err == nil || !strings.Contains(err.Error(), "needs an endpoint") {
		t.Fatalf("no endpoint at all: %v", err)
	}
}
