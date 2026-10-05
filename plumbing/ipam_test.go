package plumbing

import (
	"net"
	"testing"
)

func mustPool(t *testing.T, cidr string) *ipPool {
	t.Helper()
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatal(err)
	}
	return newIPPool(n)
}

func TestIPPoolAllocateSkipsNetworkAndBroadcast(t *testing.T) {
	p := mustPool(t, "10.0.0.0/29") // 8 addrs: .0 net, .7 bcast, 6 usable
	got := map[string]bool{}
	for i := 0; i < 6; i++ {
		ip, err := p.Allocate()
		if err != nil {
			t.Fatalf("alloc %d: %v", i, err)
		}
		if got[ip] {
			t.Fatalf("duplicate allocation %s", ip)
		}
		got[ip] = true
	}
	if got["10.0.0.0"] || got["10.0.0.7"] {
		t.Fatalf("allocated network/broadcast address: %v", got)
	}
	if _, err := p.Allocate(); err != ErrNoAvailableIP {
		t.Fatalf("expected ErrNoAvailableIP, got %v", err)
	}
}

func TestIPPoolReleaseAndReuse(t *testing.T) {
	p := mustPool(t, "10.0.0.0/30") // 2 usable
	a, _ := p.Allocate()
	b, _ := p.Allocate()
	if _, err := p.Allocate(); err != ErrNoAvailableIP {
		t.Fatal("pool should be full")
	}
	p.Release(a)
	c, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if c != a {
		t.Fatalf("expected released address %s reused, got %s", a, c)
	}
	if !p.InUse(b) {
		t.Fatal("release of one address must not affect others")
	}
	// Releasing a free address is a safe no-op.
	p.Release("10.0.0.1")
	p.Release("10.0.0.1")
}

func TestIPPoolReserve(t *testing.T) {
	p := mustPool(t, "192.168.1.0/24")
	if err := p.Reserve("192.168.1.10"); err != nil {
		t.Fatal(err)
	}
	if err := p.Reserve("192.168.1.10"); err == nil {
		t.Fatal("double reserve must fail")
	}
	if err := p.Reserve("192.168.2.10"); err != ErrInvalidSubnet {
		t.Fatalf("out-of-subnet reserve: %v", err)
	}
	if err := p.Reserve("not-an-ip"); err != ErrInvalidSubnet {
		t.Fatalf("bad ip reserve: %v", err)
	}
}
