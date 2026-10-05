package cnm

import (
	"encoding/binary"
	"fmt"
	"net"
)

// pool 是一个 IPv4 子网地址池，负责分配与回收地址。
// 分配是确定性的：总是取当前最小可用地址，便于测试与审计。
type pool struct {
	subnet *net.IPNet
	base   uint32 // 网络地址
	count  uint32 // 子网内地址总数
}

func newPool(cidr string) (*pool, error) {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("invalid subnet %q: %w", cidr, err)
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return nil, fmt.Errorf("only IPv4 subnets are supported, got %q", cidr)
	}
	ones, bits := ipnet.Mask.Size()
	if bits != 32 {
		return nil, fmt.Errorf("only IPv4 subnets are supported, got %q", cidr)
	}
	total := uint32(1) << uint(32-ones)
	if total < 4 {
		return nil, fmt.Errorf("subnet %q too small: need at least 4 addresses", cidr)
	}
	return &pool{
		subnet: ipnet,
		base:   binary.BigEndian.Uint32(ip4.Mask(ipnet.Mask)),
		count:  total,
	}, nil
}

// gateway 返回约定网关地址（子网第一个地址）。
func (p *pool) gateway() string {
	return intToIP(p.base + 1).String()
}

// usable 返回可分配地址范围 [first, last]（排除网络地址与广播地址）。
func (p *pool) usable() (first, last uint32) {
	return p.base + 1, p.base + p.count - 2
}

// allocate 从池中取一个未被占用的地址（确定性：取最小可用地址）。
// used 是已分配表（IP -> containerID）。
func (p *pool) allocate(used map[string]string) (string, error) {
	first, last := p.usable()
	for v := first; v <= last; v++ {
		s := intToIP(v).String()
		if _, taken := used[s]; !taken {
			return s, nil
		}
	}
	return "", fmt.Errorf("address pool exhausted in %s", p.subnet.String())
}

// contains 判断 ip 是否落在池的可分配范围内。
func (p *pool) contains(ip string) bool {
	parsed := net.ParseIP(ip).To4()
	if parsed == nil {
		return false
	}
	v := binary.BigEndian.Uint32(parsed)
	first, last := p.usable()
	return v >= first && v <= last
}

func intToIP(v uint32) net.IP {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return net.IP(b[:])
}
