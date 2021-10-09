#!/bin/sh
name=$(basename $(pwd))
go mod tidy
go build -o ${name}.elf -ldflags '-s -w' -trimpath .
