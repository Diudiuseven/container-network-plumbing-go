package plumbing

import "errors"

var (
	ErrNetworkExists    = errors.New("plumbing: network already exists")
	ErrNetworkNotFound  = errors.New("plumbing: network not found")
	ErrContainerExists  = errors.New("plumbing: container already attached")
	ErrNoAvailableIP    = errors.New("plumbing: no available address in pool")
	ErrPortInUse        = errors.New("plumbing: host port already in use")
	ErrInvalidSubnet    = errors.New("plumbing: invalid subnet")
	ErrInvalidPortMap   = errors.New("plumbing: invalid port mapping")
	ErrEndpointNotFound = errors.New("plumbing: endpoint not found")
)
