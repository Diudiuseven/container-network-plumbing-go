package plumbing

import "fmt"

// portKey identifies a host port binding.
type portKey struct {
	Proto string
	Port  int
}

func (k portKey) String() string { return fmt.Sprintf("%s/%d", k.Proto, k.Port) }

// portOwner records who holds a host port and which container port it maps to.
type portOwner struct {
	ContainerID   string `json:"containerId"`
	ContainerPort int    `json:"containerPort"`
}

// portAllocator tracks host ports published by containers.
type portAllocator struct {
	used map[portKey]portOwner
}

func newPortAllocator() *portAllocator {
	return &portAllocator{used: map[portKey]portOwner{}}
}

func validPortMapping(pm PortMapping) bool {
	if pm.Proto != "tcp" && pm.Proto != "udp" {
		return false
	}
	return pm.HostPort > 0 && pm.HostPort <= 65535 &&
		pm.ContainerPort > 0 && pm.ContainerPort <= 65535
}

// Reserve claims all mappings for containerID atomically; on any conflict
// or invalid mapping nothing is reserved and an error is returned.
func (a *portAllocator) Reserve(containerID string, maps []PortMapping) error {
	for _, pm := range maps {
		if !validPortMapping(pm) {
			return fmt.Errorf("%w: %+v", ErrInvalidPortMap, pm)
		}
		k := portKey{Proto: pm.Proto, Port: pm.HostPort}
		if _, taken := a.used[k]; taken {
			return fmt.Errorf("%w: %s", ErrPortInUse, k)
		}
	}
	for _, pm := range maps {
		a.used[portKey{Proto: pm.Proto, Port: pm.HostPort}] = portOwner{
			ContainerID:   containerID,
			ContainerPort: pm.ContainerPort,
		}
	}
	return nil
}

// ReleaseAll frees every port held by containerID.
func (a *portAllocator) ReleaseAll(containerID string) {
	for k, owner := range a.used {
		if owner.ContainerID == containerID {
			delete(a.used, k)
		}
	}
}

// Owner returns the container holding the given host port, if any.
func (a *portAllocator) Owner(proto string, port int) (string, bool) {
	o, ok := a.used[portKey{Proto: proto, Port: port}]
	return o.ContainerID, ok
}

// Of returns all mappings held by containerID.
func (a *portAllocator) Of(containerID string) []PortMapping {
	var out []PortMapping
	for k, o := range a.used {
		if o.ContainerID == containerID {
			out = append(out, PortMapping{
				HostPort:      k.Port,
				ContainerPort: o.ContainerPort,
				Proto:         k.Proto,
			})
		}
	}
	return out
}
