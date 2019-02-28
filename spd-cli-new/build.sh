#!/bin/sh
export GOPATH=$(pwd)

go get github.com/gorilla/websocket
go install github.com/gorilla/websocket

go get github.com/gorilla/schema
go install github.com/gorilla/schema

go build src/spd-cli.go
