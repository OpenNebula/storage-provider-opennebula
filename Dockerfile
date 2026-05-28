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
ARG BUILDPLATFORM=linux/amd64
# Build the manager binary
FROM --platform=${BUILDPLATFORM} golang:1.24 AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

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
    go build -a \
    -ldflags "-X github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/driver.driverVersion=${VERSION} -X github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/driver.driverCommit=${COMMIT} -X github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/driver.driverBuildDate=${BUILD_DATE}" \
    -o opennebula-csi cmd/opennebula-csi/main.go

###
# TARGET IMAGES
###

##
# CSI Plugin image
##
FROM alpine:3.22 AS opennebula-csi
WORKDIR /app

RUN apk add --no-cache ceph-common ceph-fuse e2fsprogs xfsprogs xfsprogs-extra util-linux udev

COPY --from=builder /workspace/opennebula-csi .

ENTRYPOINT ["/app/opennebula-csi"]
