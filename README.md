<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# norca-loadbalancer (moved)

This repository moved to
[norca-platform](https://github.com/bacanoo-io/norca-platform). The load
balancer providers are now platforms of `norca-provider`, the one provider
application of Norca, which serves node scaling and load balancers with the
same credentials. The LoadBalancer operator lives there too.

| Here | In norca-platform |
| --- | --- |
| `cmd/lbprovider` | `cmd/norca-provider` (the configuration lists providers, each with a `loadbalancer` section of classes) |
| `pkg/provider` (types, parameters) | `provider` (type registry, configuration, server), `provider/params`, `provider/lb` (the contract, its handler and client, `lbtest`) |
| `internal/providers/<type>` | `providers/<platform>` |
| `internal/registrar` | `provider/registrar` (one registrar for NodeProvider and LoadBalancerClass objects) |
| `internal/config`, `classes.example.json` | `provider/config.go`, `providers.example.json` |
| `internal/server` | `provider/server.go` |
| The operator (`norca/controller/examples/loadbalancer` of norca) | `loadbalancer/operator`, `cmd/norca-loadbalancer` |

The history of the code before the move is in this repository up to commit
`70949df`.
