#!/bin/bash
# This script provides a wrapper to run utilities from the host with proper path resolution

HOST_ROOT=${HOST_ROOT:-/host}
UTIL_NAME=$1
shift

if [ -z "$UTIL_NAME" ]; then
  echo "Usage: host-util-wrapper.sh <utility-name> [args...]"
  exit 1
fi

# Check if utility exists on host
if [ ! -x "${HOST_ROOT}/usr/local/bin/${UTIL_NAME}" ]; then
  echo "Error: Utility '${UTIL_NAME}' not found in ${HOST_ROOT}/usr/local/bin/"
  exit 1
fi

# Use chroot to run the utility in the host's root
exec chroot ${HOST_ROOT} /usr/local/bin/${UTIL_NAME} "$@" 