# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0
#
# The provider as an image: a static binary, nothing else.
#
#   CGO_ENABLED=0 GOOS=linux go build -o lbprovider ./cmd/lbprovider
#   docker build -t norca-lb/provider .
FROM scratch
COPY lbprovider /lbprovider
ENTRYPOINT ["/lbprovider"]
