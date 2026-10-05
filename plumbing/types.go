// Package plumbing provides container network management: networks, address
// allocation, port publishing, name resolution and crash recovery.
package plumbing

import "net"

// PortMapping describes a published container port on the host.
type PortMapping struct {
	HostPort      int    `json:"hostPort"`
	ContainerPort int    `json:"containerPort"`
	Proto         string `json:"proto"` // "tcp" or "udp"
}

// Endpoint is a container's attachment to one network.
type Endpoint struct {
	ContainerID string   `json:"containerId"`
	Name        string   `json:"name"`
	Aliases     []string `json:"aliases,omitempty"`
	IP          string   `json:"ip"`
}

// Network is a named L3 network with its own address pool.
type Network struct {
	Name      string              `json:"name"`
	Subnet    string              `json:"subnet"`
	Endpoints map[string]Endpoint `json:"endpoints"` // key: containerID
}

// AttachRequest describes a container to be attached to one network.
type AttachRequest struct {
	ContainerID string
	Name        string
	Aliases     []string
	Ports       []PortMapping
}

// ipInSubnet reports whether ip belongs to the subnet.
func ipInSubnet(subnet *net.IPNet, ip net.IP) bool {
	return subnet.Contains(ip)
}
