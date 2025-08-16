#!/bin/bash


set -e 

# If --clean or -c is passed, remove the generated files and exit
if [[ "$1" == "clean" ]]; then
  rm -rf proto/gen
  mkdir -p proto/gen/jobpb
  exit 0
fi

# Create output directory if it doesn't exist
if [ ! -d proto/gen/jobpb ]; then
  mkdir -p proto/gen/jobpb
fi

# Generate Go code from .proto files
protoc \
  --proto_path=proto \
  --go_out=proto/gen/jobpb \
  --go_opt=paths=source_relative \
  --go-grpc_out=proto/gen/jobpb \
  --go-grpc_opt=paths=source_relative \
  proto/job.proto
