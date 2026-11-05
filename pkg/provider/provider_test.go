// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type stub struct{ caps Capabilities }

func (s stub) Capabilities() Capabilities                   { return s.caps }
func (stub) Ensure(context.Context, Request) (State, error) { return State{}, nil }
func (stub) Delete(context.Context, Ref) error              { return nil }

var testParams = []Param{
	{Name: "endpoint", Type: ParamString, Required: true},
	{Name: "token", Type: ParamString, Secret: true, Default: ""},
	{Name: "size", Type: ParamString, Default: "small", Enum: []string{"small", "large"}, Dynamic: true},
	{Name: "retries", Type: ParamInt, Default: 3, Dynamic: true},
	{Name: "timeout", Type: ParamDuration, Default: 10 * time.Second, Dynamic: true},
	{Name: "debug", Type: ParamBool, Default: false},
	{Name: "zones", Type: ParamList, Default: []string{"a"}},
}

func TestResolve(t *testing.T) {
	got, err := Resolve(testParams, map[string]any{
		"endpoint": "https://x",
		"retries":  float64(5), // as JSON delivers numbers
		"timeout":  "45s",
		"debug":    "true",
		"zones":    []any{"a", "b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := Values{
		"endpoint": "https://x", "token": "", "size": "small", "retries": 5,
		"timeout": 45 * time.Second, "debug": true, "zones": []string{"a", "b"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
	if got.String("size") != "small" || got.Int("retries") != 5 || !got.Bool("debug") ||
		got.Duration("timeout") != 45*time.Second || len(got.List("zones")) != 2 {
		t.Fatal("typed getters")
	}
}

func TestResolveErrors(t *testing.T) {
	cases := map[string]struct {
		raw  map[string]any
		want string
	}{
		"required":  {map[string]any{}, `parameter "endpoint" is required`},
		"unknown":   {map[string]any{"endpoint": "x", "nope": 1, "also": 2}, "unknown parameters: also, nope"},
		"enum":      {map[string]any{"endpoint": "x", "size": "huge"}, "must be one of small, large"},
		"int":       {map[string]any{"endpoint": "x", "retries": 1.5}, "must be an integer"},
		"duration":  {map[string]any{"endpoint": "x", "timeout": "soon"}, "must be a duration"},
		"bool":      {map[string]any{"endpoint": "x", "debug": "maybe"}, "must be true or false"},
		"list type": {map[string]any{"endpoint": "x", "zones": []any{1}}, "must be a list of strings"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Resolve(testParams, tc.raw)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
		})
	}
}

func TestSecrets(t *testing.T) {
	t.Setenv("LB_TEST_TOKEN", "from-env")
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{"env:LB_TEST_TOKEN": "from-env", "file:" + file: "from-file", "literal": "literal"} {
		got, err := Resolve(testParams, map[string]any{"endpoint": "x", "token": in})
		if err != nil || got.String("token") != want {
			t.Fatalf("%s: got %q, %v", in, got.String("token"), err)
		}
	}
	if _, err := Resolve(testParams, map[string]any{"endpoint": "x", "token": "env:LB_TEST_UNSET"}); err == nil {
		t.Fatal("an unset variable must be an error")
	}
}

func TestOverlayAndPublished(t *testing.T) {
	base, err := Resolve(testParams, map[string]any{"endpoint": "https://x", "token": "s3cret"})
	if err != nil {
		t.Fatal(err)
	}

	// Dynamic parameters are overridden, the others and unknown names are not.
	got, err := Overlay(testParams, base, map[string]any{
		"size": "large", "retries": float64(9), "endpoint": "https://evil", "unknown": 1, "debug": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.String("size") != "large" || got.Int("retries") != 9 {
		t.Fatalf("dynamic values not applied: %#v", got)
	}
	if got.String("endpoint") != "https://x" || got.Bool("debug") {
		t.Fatalf("a fixed parameter was overridden: %#v", got)
	}
	if base.String("size") != "small" {
		t.Fatal("Overlay changed its base")
	}
	if _, err := Overlay(testParams, base, map[string]any{"size": "huge"}); err == nil {
		t.Fatal("an invalid dynamic value must be an error")
	}

	// Secrets are not published; durations are strings.
	pub := Published(testParams, base)
	if _, ok := pub["token"]; ok {
		t.Fatal("a secret was published")
	}
	if pub["timeout"] != "10s" || pub["endpoint"] != "https://x" {
		t.Fatalf("published: %#v", pub)
	}
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	r.Register(Type{ID: "alpha", Params: testParams, New: func(c Config) (Provider, error) {
		return stub{caps: Capabilities{Internal: c.Values.Bool("debug")}}, nil
	}})
	r.Register(Type{ID: "beta", New: func(Config) (Provider, error) { return stub{}, nil }})

	if ids := []string{r.Types()[0].ID, r.Types()[1].ID}; !reflect.DeepEqual(ids, []string{"alpha", "beta"}) {
		t.Fatalf("types: %v", ids)
	}

	inst, err := r.Instantiate("edge-1", "alpha", map[string]any{"endpoint": "x", "debug": true})
	if err != nil {
		t.Fatal(err)
	}
	if inst.Class != "edge-1" || !inst.Provider.Capabilities().Internal || inst.Type.ID != "alpha" {
		t.Fatalf("instance: %#v", inst)
	}

	// Two classes of one type have their own parameters.
	other, err := r.Instantiate("edge-2", "alpha", map[string]any{"endpoint": "y"})
	if err != nil || other.Values.String("endpoint") != "y" || other.Provider.Capabilities().Internal {
		t.Fatalf("second instance: %#v, %v", other, err)
	}

	if _, err := r.Instantiate("x", "gamma", nil); err == nil || !strings.Contains(err.Error(), "linked types: alpha, beta") {
		t.Fatalf("unknown type: %v", err)
	}
	if _, err := r.Instantiate("bad name", "beta", nil); err == nil {
		t.Fatal("an invalid class name must be an error")
	}
	if _, err := r.Instantiate("x", "alpha", nil); err == nil || !strings.Contains(err.Error(), "class") {
		t.Fatalf("missing required parameter: %v", err)
	}
}

func TestRegisterPanics(t *testing.T) {
	ok := func(Config) (Provider, error) { return stub{}, nil }
	cases := map[string]Type{
		"bad id":         {ID: "Bad_ID", New: ok},
		"no constructor": {ID: "x"},
		"bad default":    {ID: "x", New: ok, Params: []Param{{Name: "n", Type: ParamInt, Default: "many"}}},
		"required+default": {ID: "x", New: ok,
			Params: []Param{{Name: "n", Type: ParamString, Required: true, Default: "a"}}},
		"secret dynamic": {ID: "x", New: ok, Params: []Param{{Name: "n", Type: ParamString, Secret: true, Dynamic: true}}},
		"duplicate param": {ID: "x", New: ok,
			Params: []Param{{Name: "n", Type: ParamString}, {Name: "n", Type: ParamString}}},
	}
	for name, typ := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected a panic")
				}
			}()
			NewRegistry().Register(typ)
		})
	}

	r := NewRegistry()
	r.Register(Type{ID: "dup", New: ok})
	defer func() {
		if recover() == nil {
			t.Fatal("registering a type twice must panic")
		}
	}()
	r.Register(Type{ID: "dup", New: ok})
}
