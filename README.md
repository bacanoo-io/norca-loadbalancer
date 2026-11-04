<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# norca-loadbalancer

An example of a **load balancer provider** for the LoadBalancer operator of
Norca. Norca does not create load balancers: a service asks for one with
`expose { load_balancer { class = "..." } }`, the operator turns the request
into a `norca.io/LoadBalancer` object, and a provider, an application like this
one, creates the real load balancer. This repository shows how such an
application is built:

- the **implementations** are separate packages with their own ID, behind one
  interface;
- they **register themselves** (like `database/sql` drivers), so the binary
  contains exactly the implementations it links;
- their **parameters are declared**, so the configuration is validated, the
  defaults are applied, and some parameters can be changed while the provider
  runs;
- the process **registers its classes** in Norca (a `LoadBalancerClass` object
  per class, with a heartbeat), so the operator always knows which classes
  exist, where to call them and what they support.

The contract with the operator is described in
[Load balancer providers](https://github.com/bacanoo-io/norca-doc) (in the
Norca documentation: operator framework).

## Layout

```
provider/            the interface, the type registry, the parameter model
providers/mock/      type "mock": load balancers in memory, for trials and tests
providers/nginx/     type "nginx": TCP load balancers as nginx stream servers
providers/tcpproxy/  type "tcpproxy": TCP load balancers inside the process (round robin, failover)
providers/all/       blank imports that link the implementations into the binary
server/              the HTTP API the operator calls (many classes, one process)
registrar/           the LoadBalancerClass objects and their heartbeat
config/              the configuration file, and building the classes from it
cmd/lbprovider/      the program
```

## Types, classes and parameters

A **type** is an implementation (`nginx`, `mock`, a cloud). A **class** is an
instance of a type with its own name and parameters; the class name is what a
service puts in `class = "..."`. One process serves any number of classes, also
several of the same type (for example one nginx for the edge and one for the
internal network).

```
$ lbprovider types
nginx - TCP load balancers as nginx stream servers on this machine
    config_dir               string   [required]  directory for the generated files; ...
    advertise_address        string   [required]  address clients use to reach the load balancers
    listen_address           string   (default 0.0.0.0)  ...
    reload_command           list     (default [nginx -s reload])  ...
```

Every parameter is declared with a name, a type (`string`, `int`, `bool`,
`duration`, `list`), a description, and optionally a default, `Required`,
`Enum`, `Secret` and `Dynamic`:

| Flag | Meaning |
| --- | --- |
| `Required` | Must be configured; there is no default. |
| `Secret` | Never published. A value may be `env:NAME` or `file:/path`, so the secret is not in the configuration file. |
| `Dynamic` | Can be changed while the provider runs: the operator sends the `parameters` of the `LoadBalancerClass` object with every call, and these values override the configured ones. Edit them with `norca object apply`, no restart. |

The parameters that are not secret are published in `spec.parameters` of the
`LoadBalancerClass` object. Unknown parameters and wrong types are errors at
start; the provider never parses its own settings.

## Configuration

`classes.example.json` shows a full file. The endpoint a class registers is
`advertise_url` + `/classes/<class>`.

```json
{
  "listen": ":8443",
  "tls": { "cert": "tls.crt", "key": "tls.key" },
  "advertise_url": "https://lb-provider.service.norca:8443",
  "classes": [
    {
      "class": "nginx-edge",
      "type": "nginx",
      "token": "env:LB_NGINX_EDGE_TOKEN",
      "token_variable": "lb-provider/nginx-edge/token",
      "params": { "config_dir": "/etc/nginx/norca-lb", "advertise_address": "203.0.113.5" }
    }
  ]
}
```

- `token` is what callers must send as `Authorization: Bearer`; `token_variable`
  is the Variable in Norca, with the same value in the item `token`, that the
  operator reads to send it. A class needs a token unless `no_auth` is set
  (trusted network only).
- The operator accepts `https` endpoints; `http` only with its
  `-class-allow-http` flag.
- **`${NAME}` and `${NAME:-default}`** in any string of the file are replaced
  with the environment of the process (an unset variable without a default is
  an error), so a file can be written for a job whose address and port are only
  known when it is placed: `"listen": "0.0.0.0:${NORCA_PORT_api}"`,
  `"advertise_url": "http://${NORCA_IP_api}:18443"`. Inside a Norca job file the
  dollar sign must be doubled (`$${NORCA_PORT_api}`) in the `template` block,
  because HCL would interpolate it.
- **`endpoint`** of a class is its full URL, used instead of `advertise_url` +
  `/classes/<class>`. The URL a class registers is not the address the process
  listens on: behind a port mapping, a proxy or a load balancer of its own the
  two differ, and the provider only has to answer on the one it listens on. The
  log line of every call shows both (`host`: what the operator used, `local`:
  where it arrived).

## Running it

```
export NORCA_ADDR=https://norca.example.com:4646   # and NORCA_TOKEN
norca var put lb-provider/nginx-edge/token token="$LB_NGINX_EDGE_TOKEN"
lbprovider -config classes.json
```

As a Norca job, the provider registers on the task API socket with its workload
identity (`NORCA_ADDR=unix://${NORCA_SECRETS_DIR}/api.sock`, `identity { env =
true }`; the `unix://` address is supported). The job of the lab case
(`norca-lab`, `cases\loadbalancer\jobs\provider.norca.hcl`) is a complete
example: a static port, a configuration file written by a `template` block with
the address and port of the task, and a policy bound to the job. Run the
provider on a node whose address the operator can reach, and open its port in
the firewall.

The program registers each class (`norca.io/LoadBalancerClass`, which must be
registered as a kind first: `loadbalancerclass.kind.hcl` in the operator
example), updates `status.lastHeartbeat` every `heartbeat_interval` (default
30 seconds; the operator's `-class-ttl` is three minutes), registers a class
again if its object disappears, and removes the registrations when it exits
(`-deregister-on-exit`). `-register=false` serves the API without touching
Norca.

Permissions of the provider: `read`, `write` and `write-status` on
`norca.io/LoadBalancerClass` in the namespace `"*"` of its ACL policy (the kind
is cluster-scoped), and nothing else.

Two read-only endpoints describe the process: `GET /v1/types` (the linked types
with their parameters) and `GET /v1/classes` (the classes, capabilities and
published parameters; tokens are never listed).

## Adding an implementation

1. Create `providers/<id>/` with a package that implements `provider.Provider`.
2. Declare the parameters and register the type in `init()`:

   ```go
   package hcloud

   var params = []provider.Param{
       {Name: "token", Type: provider.ParamString, Required: true, Secret: true},
       {Name: "location", Type: provider.ParamString, Default: "fsn1"},
       {Name: "type", Type: provider.ParamString, Default: "lb11", Dynamic: true},
   }

   func init() {
       provider.Register(provider.Type{ID: "hcloud", Description: "Hetzner Cloud load balancers", Params: params, New: New})
   }

   func New(cfg provider.Config) (provider.Provider, error) { /* cfg.Values.String("token") ... */ }
   ```

3. Add a blank import to `providers/all/all.go`.

`Ensure` receives the whole desired state each time (service, port, targets,
the effective parameters) and must be idempotent; `Delete` must accept what does
not exist. An error wrapping `provider.ErrInvalid` (`provider.Invalid(...)`)
means the request cannot be satisfied; any other error is retried by the
operator. The server serializes the calls for one load balancer, so an
implementation needs locking only for state shared between load balancers. An
implementation with no state of its own, like a cloud one, should find its
resources by a name derived from the namespace and name of the load balancer, or
by a tag, so it survives a restart.

## The implementations

- **mock**: keeps the load balancers in memory. `ready` and `latency` are
  dynamic, so editing the `LoadBalancerClass` object makes the mock report an
  outage or a slow cloud.
- **tcpproxy**: a TCP listener per load balancer inside the provider; each
  connection goes to the next target (round robin) and to the next one when a
  target does not answer. No external software. The load balancers live in the
  process: after a restart they are back when the operator calls again (within a
  minute for one that is not ready). The listeners bind on the machine of the
  provider; the firewall must allow their ports.
- **nginx**: writes one file per load balancer into `config_dir`, to be included
  in the `stream` context (`include /etc/nginx/norca-lb/*.conf;`), and runs
  `reload_command` when something changed. The directory is its state: a
  restarted provider continues where it was. A configuration that nginx rejects
  is rolled back; two load balancers cannot take the same port.

A cloud implementation (Hetzner, Scaleway, AWS, Google) is not part of this
example; it is a new package like `nginx`.

## Tests

```
go test ./...
```

The tests need no cloud, no nginx and no Norca: the reload command is replaced,
and the registrar is tested against a fake of the Norca API (and the request
paths against the API client).

The whole chain, with the operator, a real cluster, ACLs, three nodes, the
registered port that differs from the listening one, a wrong token, traffic
through the load balancer, a provider that stops, and the removal of a service,
is the case `loadbalancer` of the `norca-lab` repository
(`.\run-tests.ps1 -Cases loadbalancer`).

## Status

An example, not a product. The Norca API client comes from the checkout next to
this repository (`replace` in `go.mod`).
