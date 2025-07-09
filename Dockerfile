# Copyright 2025, OpenNebula Project, OpenNebula Systems.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

##
# Builder
##

# Build the manager binary
FROM --platform=${BUILDPLATFORM} golang:1.22 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY cmd/ cmd/
COPY pkg/ pkg/

# Build
# the GOARCH has not a default value to allow the binary be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -o opennebula-cloud-controller-manager cmd/opennebula-cloud-controller-manager/main.go

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -o opennebula-csi-plugin cmd/opennebula-csi-plugin/main.go

###
# TARGET IMAGES
###

##
# Cloud Controller Manager image
##

FROM gcr.io/distroless/static:nonroot AS cloud-provider-opennebula
WORKDIR /
COPY --from=builder /workspace/opennebula-cloud-controller-manager .
# Use a non-root user to run the container
USER 65532:65532

ENTRYPOINT ["/opennebula-cloud-controller-manager"]

##
# CSI Plugin image
##

FROM gcr.io/distroless/static:nonroot AS opennebula-csi-plugin
WORKDIR /
COPY --from=builder /workspace/opennebula-csi-plugin .
# Use a non-root user to run the container
USER 65532:65532

ENTRYPOINT ["/opennebula-csi-plugin"]
