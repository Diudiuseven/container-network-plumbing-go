package plumbing

import (
	"encoding/binary"
	"net"
)

// ipPool allocates addresses out of a subnet, avoiding conflicts.
// Only IPv4 subnets are supported for allocation; the network and broadcast
// addresses are never handed out.
type ipPool struct {
	subnet *net.IPNet
	used   map[string]bool // allocated addresses (string form)
}

func newIPPool(subnet *net.IPNet) *ipPool {
	return &ipPool{subnet: subnet, used: map[string]bool{}}
}

// Allocate returns a free address, or ErrNoAvailableIP when the pool is full.
func (p *ipPool) Allocate() (string, error) {
	ones, bits := p.subnet.Mask.Size()
	if bits != 32 {
		return "", ErrInvalidSubnet
	}
	base := binary.BigEndian.Uint32(p.subnet.IP.To4())
	count := uint32(1) << uint(32-ones)
	// Skip network address (first) and broadcast (last) for subnets that
	// have host room; tiny subnets (/31, /32) yield nothing.
	if count <= 2 {
		return "", ErrNoAvailableIP
	}
	for i := uint32(1); i < count-1; i++ {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], base+i)
		s := net.IP(b[:]).String()
		if !p.used[s] {
			p.used[s] = true
			return s, nil
		}
	}
	return "", ErrNoAvailableIP
}

// Reserve marks ip as used; fails if already taken or outside the subnet.
func (p *ipPool) Reserve(ip string) error {
	parsed := net.ParseIP(ip)
	if parsed == nil || !ipInSubnet(p.subnet, parsed) {
		return ErrInvalidSubnet
	}
	if p.used[ip] {
		return ErrNoAvailableIP
	}
	p.used[ip] = true
	return nil
}

// Release frees ip; releasing a free address is a no-op so recovery can
// safely replay releases.
func (p *ipPool) Release(ip string) {
	delete(p.used, ip)
}

// InUse reports whether ip is currently allocated.
func (p *ipPool) InUse(ip string) bool { return p.used[ip] }
