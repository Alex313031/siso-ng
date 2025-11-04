#!/bin/bash

go build -o ./release/ -a -ldflags '-s -w -extldflags "-static"' -v .
