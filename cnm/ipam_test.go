package cnm

import "testing"

func TestPoolAllocateDeterministic(t *testing.T) {
	p, err := newPool("10.10.0.0/24")
	if err != nil {
		t.Fatalf("newPool: %v", err)
	}
	used := map[string]string{}
	ip1, err := p.allocate(used)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if ip1 != "10.10.0.1" {
		t.Fatalf("first allocation = %s, want 10.10.0.1", ip1)
	}
	used[ip1] = "c1"
	ip2, err := p.allocate(used)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if ip2 != "10.10.0.2" {
		t.Fatalf("second allocation = %s, want 10.10.0.2", ip2)
	}
}

func TestPoolReuseAfterRelease(t *testing.T) {
	p, _ := newPool("10.10.0.0/30")
	used := map[string]string{}
	a, _ := p.allocate(used)
	used[a] = "c1"
	b, _ := p.allocate(used)
	used[b] = "c2"
	// /30 只有 2 个可用地址，此时应耗尽。
	if _, err := p.allocate(used); err == nil {
		t.Fatal("expected pool exhausted error")
	}
	// 释放 c1 的地址后可回收，且不影响 c2。
	delete(used, a)
	got, err := p.allocate(used)
	if err != nil {
		t.Fatalf("allocate after release: %v", err)
	}
	if got != a {
		t.Fatalf("reused address = %s, want %s", got, a)
	}
	if used[b] != "c2" {
		t.Fatalf("in-use address %s disturbed", b)
	}
}

func TestPoolRejectsInvalid(t *testing.T) {
	for _, cidr := range []string{"bad", "10.0.0.0/33", "fd00::/64", "10.0.0.0/31"} {
		if _, err := newPool(cidr); err == nil {
			t.Fatalf("newPool(%q) should fail", cidr)
		}
	}
}

func TestPoolContains(t *testing.T) {
	p, _ := newPool("192.168.1.0/24")
	if !p.contains("192.168.1.10") {
		t.Fatal("should contain 192.168.1.10")
	}
	if p.contains("192.168.2.10") {
		t.Fatal("should not contain 192.168.2.10")
	}
	if p.contains("192.168.1.0") || p.contains("192.168.1.255") {
		t.Fatal("network/broadcast addresses are not usable")
	}
}
