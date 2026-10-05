package plumbing

import (
	"context"
	"errors"
	"testing"
)

func openTemp(t *testing.T) *Manager {
	t.Helper()
	m, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCreateNetworkIdempotent(t *testing.T) {
	m := openTemp(t)
	ctx := context.Background()
	if err := m.CreateNetwork(ctx, "web", "10.1.0.0/24"); err != nil {
		t.Fatal(err)
	}
	// Same subnet: no-op.
	if err := m.CreateNetwork(ctx, "web", "10.1.0.0/24"); err != nil {
		t.Fatalf("duplicate create must be a no-op: %v", err)
	}
	// Different subnet: conflict.
	if err := m.CreateNetwork(ctx, "web", "10.2.0.0/24"); !errors.Is(err, ErrNetworkExists) {
		t.Fatalf("expected ErrNetworkExists, got %v", err)
	}
	if err := m.CreateNetwork(ctx, "bad", "not-a-cidr"); !errors.Is(err, ErrInvalidSubnet) {
		t.Fatalf("expected ErrInvalidSubnet, got %v", err)
	}
}

func TestDeleteNetworkIdempotentAndCleansUp(t *testing.T) {
	m := openTemp(t)
	ctx := context.Background()
	_ = m.CreateNetwork(ctx, "web", "10.1.0.0/24")
	ep, err := m.Attach(ctx, "web", AttachRequest{
		ContainerID: "c1", Name: "app",
		Ports: []PortMapping{{HostPort: 8080, ContainerPort: 80, Proto: "tcp"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ip := ep.IP
	if err := m.DeleteNetwork(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	// Double delete is a no-op.
	if err := m.DeleteNetwork(ctx, "web"); err != nil {
		t.Fatalf("double delete must be a no-op: %v", err)
	}
	// Ports must be released.
	if ports, _ := m.PortsOf(ctx, "c1"); len(ports) != 0 {
		t.Fatalf("ports leaked after network delete: %v", ports)
	}
	// Recreate the network: the old address must be reusable, no residue.
	if err := m.CreateNetwork(ctx, "web", "10.1.0.0/24"); err != nil {
		t.Fatal(err)
	}
	ep2, err := m.Attach(ctx, "web", AttachRequest{ContainerID: "c2", Name: "app2"})
	if err != nil {
		t.Fatal(err)
	}
	if ep2.IP != ip {
		t.Fatalf("expected address %s to be recycled, got %s", ip, ep2.IP)
	}
	// Port must be reusable too.
	if _, err := m.Attach(ctx, "web", AttachRequest{
		ContainerID: "c3", Name: "app3",
		Ports: []PortMapping{{HostPort: 8080, ContainerPort: 80, Proto: "tcp"}},
	}); err != nil {
		t.Fatalf("port 8080 should be reusable after delete: %v", err)
	}
}

func TestAttachDetachLifecycle(t *testing.T) {
	m := openTemp(t)
	ctx := context.Background()
	_ = m.CreateNetwork(ctx, "web", "10.1.0.0/24")
	ep, err := m.Attach(ctx, "web", AttachRequest{ContainerID: "c1", Name: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if ep.IP == "" {
		t.Fatal("endpoint must have an address")
	}
	// Duplicate attach on the same network fails.
	if _, err := m.Attach(ctx, "web", AttachRequest{ContainerID: "c1", Name: "app"}); !errors.Is(err, ErrContainerExists) {
		t.Fatalf("expected ErrContainerExists, got %v", err)
	}
	// Attach to a missing network fails.
	if _, err := m.Attach(ctx, "nope", AttachRequest{ContainerID: "c2"}); !errors.Is(err, ErrNetworkNotFound) {
		t.Fatalf("expected ErrNetworkNotFound, got %v", err)
	}
	if err := m.Detach(ctx, "web", "c1"); err != nil {
		t.Fatal(err)
	}
	// Double detach is a no-op.
	if err := m.Detach(ctx, "web", "c1"); err != nil {
		t.Fatalf("double detach must be a no-op: %v", err)
	}
	// Address is recycled for the next container.
	ep2, err := m.Attach(ctx, "web", AttachRequest{ContainerID: "c2", Name: "app2"})
	if err != nil {
		t.Fatal(err)
	}
	if ep2.IP != ep.IP {
		t.Fatalf("expected recycled address %s, got %s", ep.IP, ep2.IP)
	}
}

func TestAttachAllAtomicAcrossNetworks(t *testing.T) {
	m := openTemp(t)
	ctx := context.Background()
	_ = m.CreateNetwork(ctx, "a", "10.1.0.0/24")
	_ = m.CreateNetwork(ctx, "b", "10.2.0.0/24")
	// One container occupies the only usable address of network "b".
	_ = m.CreateNetwork(ctx, "tiny", "10.3.0.0/30")
	if _, err := m.Attach(ctx, "tiny", AttachRequest{ContainerID: "x1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Attach(ctx, "tiny", AttachRequest{ContainerID: "x2"}); err != nil {
		t.Fatal(err)
	}
	// Attaching to [a, tiny] must fail on tiny and leave nothing on a.
	_, err := m.AttachAll(ctx, []string{"a", "tiny"}, AttachRequest{
		ContainerID: "c1", Name: "app",
		Ports: []PortMapping{{HostPort: 8080, ContainerPort: 80, Proto: "tcp"}},
	})
	if !errors.Is(err, ErrNoAvailableIP) {
		t.Fatalf("expected ErrNoAvailableIP, got %v", err)
	}
	n, _ := m.Network(ctx, "a")
	if len(n.Endpoints) != 0 {
		t.Fatalf("partial attach left on network a: %v", n.Endpoints)
	}
	if ports, _ := m.PortsOf(ctx, "c1"); len(ports) != 0 {
		t.Fatalf("ports leaked after failed AttachAll: %v", ports)
	}
	// The container can still attach cleanly afterwards.
	eps, err := m.AttachAll(ctx, []string{"a", "b"}, AttachRequest{ContainerID: "c1", Name: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 2 || eps[0].IP == eps[1].IP {
		t.Fatalf("expected one address per network, got %v", eps)
	}
}

func TestPortConflictRejectedBeforeAttach(t *testing.T) {
	m := openTemp(t)
	ctx := context.Background()
	_ = m.CreateNetwork(ctx, "web", "10.1.0.0/24")
	if _, err := m.Attach(ctx, "web", AttachRequest{
		ContainerID: "c1",
		Ports:       []PortMapping{{HostPort: 8080, ContainerPort: 80, Proto: "tcp"}},
	}); err != nil {
		t.Fatal(err)
	}
	_, err := m.Attach(ctx, "web", AttachRequest{
		ContainerID: "c2",
		Ports:       []PortMapping{{HostPort: 8080, ContainerPort: 8080, Proto: "tcp"}},
	})
	if !errors.Is(err, ErrPortInUse) {
		t.Fatalf("expected ErrPortInUse, got %v", err)
	}
	// The rejected container must hold no address.
	n, _ := m.Network(ctx, "web")
	if _, ok := n.Endpoints["c2"]; ok {
		t.Fatal("rejected container must not be attached")
	}
	// Detaching the owner frees the port for reuse.
	if err := m.Detach(ctx, "web", "c1"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Attach(ctx, "web", AttachRequest{
		ContainerID: "c2",
		Ports:       []PortMapping{{HostPort: 8080, ContainerPort: 8080, Proto: "tcp"}},
	}); err != nil {
		t.Fatalf("port should be free after owner detached: %v", err)
	}
}

func TestResolveByNameAndAlias(t *testing.T) {
	m := openTemp(t)
	ctx := context.Background()
	_ = m.CreateNetwork(ctx, "web", "10.1.0.0/24")
	ep1, _ := m.Attach(ctx, "web", AttachRequest{ContainerID: "c1", Name: "web", Aliases: []string{"frontend"}})
	ep2, _ := m.Attach(ctx, "web", AttachRequest{ContainerID: "c2", Name: "web"})
	_, _ = m.Attach(ctx, "web", AttachRequest{ContainerID: "c3", Name: "db"})

	ips, err := m.Resolve(ctx, "web", "web")
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) != 2 {
		t.Fatalf("expected 2 addresses for scaled service, got %v", ips)
	}
	ips, err = m.Resolve(ctx, "web", "frontend")
	if err != nil || len(ips) != 1 || ips[0] != ep1.IP {
		t.Fatalf("alias resolve: %v %v", ips, err)
	}
	// Scale down: detached container must disappear from results.
	if err := m.Detach(ctx, "web", "c1"); err != nil {
		t.Fatal(err)
	}
	ips, _ = m.Resolve(ctx, "web", "web")
	if len(ips) != 1 || ips[0] != ep2.IP {
		t.Fatalf("stale address returned after detach: %v", ips)
	}
	ips, _ = m.Resolve(ctx, "web", "frontend")
	if len(ips) != 0 {
		t.Fatalf("alias of offline container must not resolve: %v", ips)
	}
	// Resolution is per-network.
	if _, err := m.Resolve(ctx, "missing", "web"); !errors.Is(err, ErrNetworkNotFound) {
		t.Fatalf("expected ErrNetworkNotFound, got %v", err)
	}
}
