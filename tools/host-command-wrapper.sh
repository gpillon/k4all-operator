#!/bin/bash

# This script proxies commands to the host if they don't exist in the container
# Usage: host-command [command] [args...]

# Directory to store symlinks to this wrapper
WRAPPER_DIR="/usr/local/host-bin"
HOST_PATH="/host/usr/bin:/host/bin:/host/usr/sbin:/host/sbin"

# Function to proxy command to host
proxy_command() {
    cmd="$1"
    shift
    
    # Find the command on the host
    host_cmd=""
    for dir in $(echo "$HOST_PATH" | tr ':' ' '); do
        if [ -x "${dir}/${cmd}" ]; then
            host_cmd="${dir}/${cmd}"
            break
        fi
    done
    
    if [ -z "$host_cmd" ]; then
        echo "Error: Command '$cmd' not found in container or on host" >&2
        return 127
    fi
    
    # Execute the host command with all arguments
    exec "$host_cmd" "$@"
}

# If called directly, proxy the first argument as command
if [ "$(basename "$0")" = "host-command" ]; then
    if [ $# -eq 0 ]; then
        echo "Usage: host-command [command] [args...]" >&2
        exit 1
    fi
    
    proxy_command "$@"
else
    # Called via symlink - the command is the name of the symlink
    cmd=$(basename "$0")
    proxy_command "$cmd" "$@"
fi 