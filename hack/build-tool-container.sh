#! /bin/bash

# Build the tool container
# This is used to build the tool container and push it to the registry

#set the container tool:
TOOL_CONTAINER_EXECUTABLE=podman
TOOL_CONTAINER_REGISTRY="ghcr.io/gpillon/k4all-tools"
TOOL_CONTAINER_TAG="latest"
TOOL_CONTAINER_OS="fedora"

set -e

echo "Building tool container"
${TOOL_CONTAINER_EXECUTABLE} build -t ${TOOL_CONTAINER_REGISTRY}:${TOOL_CONTAINER_TAG} -f tools/Dockerfile.${TOOL_CONTAINER_OS} .

echo "Pushing tool container"
${TOOL_CONTAINER_EXECUTABLE} push ${TOOL_CONTAINER_REGISTRY}:${TOOL_CONTAINER_TAG}