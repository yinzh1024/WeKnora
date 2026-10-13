#!/bin/bash
set -e
cd /media/yinzh/ext/WeKnora
# /home only has ~3GB free — keep GOPATH/GOCACHE on the ext disk.
export GOPATH=/media/yinzh/ext/gopath
export GOCACHE=/media/yinzh/ext/gocache
export GOPROXY=https://goproxy.cn,direct
export CGO_ENABLED=1
bt=$(date +%Y-%m-%dT%H:%M:%S%z)
go build -tags "sqlite_fts5" -ldflags "-w -s \
  -X github.com/Tencent/WeKnora/internal/handler.Version=0.8.2 \
  -X github.com/Tencent/WeKnora/internal/handler.Edition=lite \
  -X github.com/Tencent/WeKnora/internal/handler.CommitID=f7166b65b \
  -X github.com/Tencent/WeKnora/internal/handler.BuildTime=$bt \
  -X google.golang.org/protobuf/reflect/protoregistry.conflictPolicy=warn" \
  -o WeKnora-lite ./cmd/server
echo BUILD_OK $(date +%T)
