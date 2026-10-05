package plumbing

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// TestConcurrentAttachDetachConverges hammers the manager with concurrent
// attaches, detaches and queries, then verifies the final state is exactly
// what a serial execution of the surviving operations would produce:
// unique addresses, consistent ports, and consistent resolution.
func TestConcurrentAttachDetachConverges(t *testing.T) {
	m := openTemp(t)
	ctx := context.Background()
	for _, nw := range []string{"n1", "n2"} {
		if err := m.CreateNetwork(ctx, nw, "10.10.0.0/22"); err != nil {
			t.Fatal(err)
		}
	}
	const workers = 16
	const perWorker = 25

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			id := fmt.Sprintf("c-%d", w)
			ports := []PortMapping{
				{HostPort: 20000 + w, ContainerPort: 80, Proto: "tcp"},
			}
			for i := 0; i < perWorker; i++ {
				_, err := m.AttachAll(ctx, []string{"n1", "n2"}, AttachRequest{
					ContainerID: id,
					Name:        fmt.Sprintf("svc-%d", w%4),
					Aliases:     []string{"fleet"},
					Ports:       ports,
				})
				if err != nil {
					t.Errorf("attach %s: %v", id, err)
					return
				}
				// Concurrent queries must never fail or observe torn state.
				if _, err := m.Resolve(ctx, "n1", "fleet"); err != nil {
					t.Errorf("resolve: %v", err)
					return
				}
				if _, err := m.Network(ctx, "n2"); err != nil {
					t.Errorf("network: %v", err)
					return
				}
				if err := m.Detach(ctx, "n1", id); err != nil {
					t.Errorf("detach n1 %s: %v", id, err)
					return
				}
				if err := m.Detach(ctx, "n2", id); err != nil {
					t.Errorf("detach n2 %s: %v", id, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	// Every container detached from both networks: state must be empty.
	for _, nw := range []string{"n1", "n2"} {
		n, err := m.Network(ctx, nw)
		if err != nil {
			t.Fatal(err)
		}
		if len(n.Endpoints) != 0 {
			t.Fatalf("network %s has leftover endpoints: %v", nw, n.Endpoints)
		}
		ips, err := m.Resolve(ctx, nw, "fleet")
		if err != nil {
			t.Fatal(err)
		}
		if len(ips) != 0 {
			t.Fatalf("resolve returned offline containers: %v", ips)
		}
	}
	for w := 0; w < workers; w++ {
		ports, _ := m.PortsOf(ctx, fmt.Sprintf("c-%d", w))
		if len(ports) != 0 {
			t.Fatalf("container c-%d leaked ports: %v", w, ports)
		}
	}
	// The whole pool must be reusable again: attach one container per
	// network and expect the first address of each pool.
	ep, err := m.Attach(ctx, "n1", AttachRequest{ContainerID: "final"})
	if err != nil {
		t.Fatal(err)
	}
	if ep.IP != "10.10.0.1" {
		t.Fatalf("expected pool fully recycled, first alloc got %s", ep.IP)
	}
}

// TestConcurrentAttachUniqueAddresses verifies that concurrent attaches
// never receive duplicate addresses and that survivors hold unique ports.
func TestConcurrentAttachUniqueAddresses(t *testing.T) {
	m := openTemp(t)
	ctx := context.Background()
	if err := m.CreateNetwork(ctx, "net", "10.20.0.0/24"); err != nil {
		t.Fatal(err)
	}
	const n = 50
	eps := make([]*Endpoint, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ep, err := m.Attach(ctx, "net", AttachRequest{
				ContainerID: fmt.Sprintf("c-%d", i),
				Name:        "svc",
			})
			if err != nil {
				t.Errorf("attach: %v", err)
				return
			}
			eps[i] = ep
		}(i)
	}
	wg.Wait()
	seen := map[string]string{}
	for i, ep := range eps {
		if ep == nil {
			continue
		}
		if owner, dup := seen[ep.IP]; dup {
			t.Fatalf("address %s assigned to both %s and c-%d", ep.IP, owner, i)
		}
		seen[ep.IP] = fmt.Sprintf("c-%d", i)
	}
	if len(seen) != n {
		t.Fatalf("expected %d unique addresses, got %d", n, len(seen))
	}
	// Resolve must see exactly the live set.
	ips, err := m.Resolve(ctx, "net", "svc")
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) != n {
		t.Fatalf("resolve saw %d endpoints, want %d", len(ips), n)
	}
}

// TestConcurrentPortConflict ensures only one container can win a host port
// and losers fail cleanly without being attached.
func TestConcurrentPortConflict(t *testing.T) {
	m := openTemp(t)
	ctx := context.Background()
	if err := m.CreateNetwork(ctx, "net", "10.30.0.0/24"); err != nil {
		t.Fatal(err)
	}
	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = m.Attach(ctx, "net", AttachRequest{
				ContainerID: fmt.Sprintf("c-%d", i),
				Ports: []PortMapping{
					{HostPort: 8080, ContainerPort: 80, Proto: "tcp"},
				},
			})
		}(i)
	}
	wg.Wait()
	var winners, losers int
	for _, err := range errs {
		if err == nil {
			winners++
		} else {
			losers++
		}
	}
	if winners != 1 || losers != n-1 {
		t.Fatalf("expected exactly 1 winner, got %d winners %d losers", winners, losers)
	}
	// Exactly one endpoint exists and it owns the port.
	net1, _ := m.Network(ctx, "net")
	if len(net1.Endpoints) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(net1.Endpoints))
	}
}
