// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca-loadbalancer/internal/providers/mock"
	"github.com/bacanoo-io/norca-loadbalancer/internal/server"
	"github.com/bacanoo-io/norca-loadbalancer/pkg/provider"
)

type fixture struct {
	srv  *httptest.Server
	mock *mock.Provider
}

func newFixture(t *testing.T, token string, params map[string]any) *fixture {
	t.Helper()
	reg := provider.NewRegistry()
	reg.Register(provider.Type{ID: mock.TypeID, Description: "mock", Params: mockParams(t), New: mock.New})
	inst, err := reg.Instantiate("mock-1", mock.TypeID, params)
	if err != nil {
		t.Fatal(err)
	}
	s := server.New([]*server.Class{{Instance: inst, Token: token}}, reg.Types(), nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &fixture{srv: ts, mock: inst.Provider.(*mock.Provider)}
}

// mockParams returns the declarations of the mock type by registering it in
// a scratch registry: the type registers itself in Default from init.
func mockParams(t *testing.T) []provider.Param {
	typ, ok := provider.Default.Lookup(mock.TypeID)
	if !ok {
		t.Fatal("the mock type did not register itself")
	}
	return typ.Params
}

func (f *fixture) do(t *testing.T, method, path, token, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, f.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

const lbPath = "/classes/mock-1/v1/loadbalancers/default/api"

const body = `{"name":"api","namespace":"default","service":"api","port":443,
 "targets":[{"address":"10.0.0.2","port":8001,"allocID":"b"},{"address":"10.0.0.1","port":8001,"allocID":"a"}]}`

func TestPutAndDelete(t *testing.T) {
	f := newFixture(t, "s3cret", map[string]any{"address": "203.0.113.7"})

	status, out := f.do(t, http.MethodPut, lbPath, "s3cret", body)
	if status != http.StatusOK || out["ready"] != true || out["resource"] != "mock/mock-1/default/api" {
		t.Fatalf("put: %d %v", status, out)
	}
	if addrs := out["addresses"].([]any); len(addrs) != 1 || addrs[0] != "203.0.113.7:443" {
		t.Fatalf("addresses: %v", addrs)
	}
	snap := f.mock.Snapshot()
	if len(snap) != 1 || len(snap[0].Targets) != 2 || snap[0].Targets[0] != "10.0.0.1:8001" {
		t.Fatalf("state: %+v", snap)
	}

	// PUT is idempotent.
	if status, _ = f.do(t, http.MethodPut, lbPath, "s3cret", body); status != http.StatusOK || len(f.mock.Snapshot()) != 1 {
		t.Fatal("a second put must not create a second load balancer")
	}

	if status, _ = f.do(t, http.MethodDelete, lbPath, "s3cret", ""); status != http.StatusNoContent {
		t.Fatalf("delete: %d", status)
	}
	if len(f.mock.Snapshot()) != 0 {
		t.Fatal("the load balancer was not deleted")
	}
	// Deleting what does not exist is not an error.
	if status, _ = f.do(t, http.MethodDelete, lbPath, "s3cret", ""); status != http.StatusNoContent {
		t.Fatalf("second delete: %d", status)
	}
}

func TestAuth(t *testing.T) {
	f := newFixture(t, "s3cret", nil)
	for name, token := range map[string]string{"none": "", "wrong": "nope"} {
		if status, _ := f.do(t, http.MethodPut, lbPath, token, body); status != http.StatusUnauthorized {
			t.Fatalf("%s token: %d", name, status)
		}
	}
	if len(f.mock.Snapshot()) != 0 {
		t.Fatal("an unauthorized call changed the state")
	}

	// A class without a token accepts calls (trusted network).
	open := newFixture(t, "", nil)
	if status, _ := open.do(t, http.MethodPut, lbPath, "", body); status != http.StatusOK {
		t.Fatalf("no-auth class: %d", status)
	}
}

func TestValidation(t *testing.T) {
	f := newFixture(t, "", map[string]any{"internal": false})
	cases := map[string]struct {
		method, path, body string
		want               int
	}{
		"unknown class":  {http.MethodPut, "/classes/other/v1/loadbalancers/default/api", body, http.StatusNotFound},
		"bad port":       {http.MethodPut, lbPath, `{"port":70000}`, http.StatusUnprocessableEntity},
		"bad target":     {http.MethodPut, lbPath, `{"port":80,"targets":[{"address":"","port":1}]}`, http.StatusUnprocessableEntity},
		"internal":       {http.MethodPut, lbPath, `{"port":80,"internal":true}`, http.StatusUnprocessableEntity},
		"bad json":       {http.MethodPut, lbPath, `{`, http.StatusBadRequest},
		"name mismatch":  {http.MethodPut, lbPath, `{"name":"other","port":80}`, http.StatusBadRequest},
		"path traversal": {http.MethodPut, "/classes/mock-1/v1/loadbalancers/default/..%2Fx", `{"port":80}`, http.StatusBadRequest},
		"bad dynamic":    {http.MethodPut, lbPath, `{"port":80,"parameters":{"latency":"soon"}}`, http.StatusUnprocessableEntity},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if status, out := f.do(t, tc.method, tc.path, "", tc.body); status != tc.want {
				t.Fatalf("status %d (%v), want %d", status, out, tc.want)
			}
		})
	}
}

// TestDynamicParameters: the parameters the operator sends back override the
// dynamic ones of the configuration, on the next call, without a restart.
func TestDynamicParameters(t *testing.T) {
	f := newFixture(t, "", nil)

	_, out := f.do(t, http.MethodPut, lbPath, "", body)
	if out["ready"] != true {
		t.Fatalf("ready by default: %v", out)
	}
	_, out = f.do(t, http.MethodPut, lbPath, "",
		`{"port":443,"targets":[{"address":"10.0.0.1","port":1}],"parameters":{"ready":false}}`)
	if out["ready"] != false || out["message"] == "" {
		t.Fatalf("the dynamic parameter was not applied: %v", out)
	}
	// A fixed parameter in the body does not override the configuration.
	_, out = f.do(t, http.MethodPut, lbPath, "",
		`{"port":443,"targets":[{"address":"10.0.0.1","port":1}],"parameters":{"address":"6.6.6.6"}}`)
	if addrs := out["addresses"].([]any); addrs[0] != "192.0.2.10:443" {
		t.Fatalf("a fixed parameter was overridden: %v", addrs)
	}
}

func TestIntrospection(t *testing.T) {
	f := newFixture(t, "s3cret", nil)

	resp, err := http.Get(f.srv.URL + "/v1/types")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var types []struct {
		ID         string `json:"id"`
		Parameters []struct {
			Name    string `json:"name"`
			Dynamic bool   `json:"dynamic"`
		} `json:"parameters"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&types); err != nil {
		t.Fatal(err)
	}
	if len(types) != 1 || types[0].ID != "mock" || len(types[0].Parameters) == 0 {
		t.Fatalf("types: %+v", types)
	}

	resp2, err := http.Get(f.srv.URL + "/v1/classes")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var classes []map[string]any
	if err := json.NewDecoder(resp2.Body).Decode(&classes); err != nil {
		t.Fatal(err)
	}
	if len(classes) != 1 || classes[0]["class"] != "mock-1" || classes[0]["tokenChecked"] != true {
		t.Fatalf("classes: %v", classes)
	}
	if _, leaked := classes[0]["token"]; leaked {
		t.Fatal("the token is listed")
	}
}
