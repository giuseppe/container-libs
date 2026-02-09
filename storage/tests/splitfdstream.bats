#!/usr/bin/env bats

load helpers

@test "splitfdstream json-rpc-server and apply-splitfdstream" {
	case "$STORAGE_DRIVER" in
	overlay*)
		;;
	*)
		skip "driver $STORAGE_DRIVER does not support splitfdstream"
		;;
	esac

	# Create and populate a test layer
	populate

	# Start the JSON-RPC server in the background
	run storage json-rpc-server &
	SERVER_PID=$!
	# Give the server time to start
	sleep 2

	# Get the socket path from runroot
	local runroot=`storage status 2>&1 | awk '/^Run Root:/{print $3}'`
	local socket_path="$runroot/json-rpc.sock"

	# Wait for socket to be created (max 10 seconds)
	local count=0
	while [[ ! -S "$socket_path" && $count -lt 50 ]]; do
		sleep 0.2
		count=$((count + 1))
	done

	# Check that the socket exists
	[ -S "$socket_path" ]

	# Create a new layer using apply-splitfdstream
	# This should connect to our JSON-RPC server and fetch the layer
	run storage apply-splitfdstream --socket "$socket_path" test-layer-id
	echo "apply-splitfdstream output: $output"
	[ "$status" -eq 0 ]
	[ "$output" != "" ]

	applied_layer="$output"

	# Verify the layer was created
	run storage layers
	[ "$status" -eq 0 ]
	[[ "$output" =~ "$applied_layer" ]]

	# Check that we can mount the applied layer
	run storage mount "$applied_layer"
	[ "$status" -eq 0 ]
	[ "$output" != "" ]
	local applied_mount="$output"

	# Verify some expected content exists (from populate function)
	[ -f "$applied_mount/layer1file1" ]
	[ -f "$applied_mount/layer1file2" ]
	[ -d "$applied_mount/layerdir1" ]

	# Unmount the layer
	run storage unmount "$applied_layer"
	[ "$status" -eq 0 ]

	# Clean up - kill the server
	if [[ -n "$SERVER_PID" ]]; then
		kill $SERVER_PID 2>/dev/null || true
		wait $SERVER_PID 2>/dev/null || true
	fi
}

@test "splitfdstream server socket path uses runroot" {
	case "$STORAGE_DRIVER" in
	overlay*)
		;;
	*)
		skip "driver $STORAGE_DRIVER does not support splitfdstream"
		;;
	esac

	# Start the JSON-RPC server in the background
	run storage json-rpc-server &
	SERVER_PID=$!
	# Give the server time to start
	sleep 2

	# Get the expected socket path from runroot
	local runroot=`storage status 2>&1 | awk '/^Run Root:/{print $3}'`
	local expected_socket="$runroot/json-rpc.sock"

	# Verify the socket is created in the correct location
	[ -S "$expected_socket" ]

	# Clean up - kill the server
	if [[ -n "$SERVER_PID" ]]; then
		kill $SERVER_PID 2>/dev/null || true
		wait $SERVER_PID 2>/dev/null || true
	fi
}