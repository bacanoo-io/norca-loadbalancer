// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package all links every implementation into the binary. An implementation
// registers itself in init(); to add one, give it a directory under
// internal/providers/ and a blank import here. A build with fewer
// implementations imports only the ones it wants.
package all

import (
	_ "github.com/bacanoo-io/norca-loadbalancer/internal/providers/aws"
	_ "github.com/bacanoo-io/norca-loadbalancer/internal/providers/digitalocean"
	_ "github.com/bacanoo-io/norca-loadbalancer/internal/providers/gcp"
	_ "github.com/bacanoo-io/norca-loadbalancer/internal/providers/hcloud"
	_ "github.com/bacanoo-io/norca-loadbalancer/internal/providers/mock"
	_ "github.com/bacanoo-io/norca-loadbalancer/internal/providers/nginx"
	_ "github.com/bacanoo-io/norca-loadbalancer/internal/providers/scaleway"
	_ "github.com/bacanoo-io/norca-loadbalancer/internal/providers/tcpproxy"
	_ "github.com/bacanoo-io/norca-loadbalancer/internal/providers/vultr"
)
