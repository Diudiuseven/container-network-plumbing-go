package cnm

import (
	"strings"
	"testing"
)

func openTemp(t *testing.T) *Manager {
	t.Helper()
	m, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return m
}

func mustCreate(t *testing.T, m *Manager, name, subnet string) {
	t.Helper()
	if err := m.CreateNetwork(name, subnet); err != nil {
		t.Fatalf("CreateNetwork(%s): %v", name, err)
	}
}

func mustAttach(t *testing.T, m *Manager, id, name string, reqs ...AttachRequest) {
	t.Helper()
	if err := m.Attach(id, name, reqs); err != nil {
		t.Fatalf("Attach(%s): %v", id, err)
	}
}

func TestCreateNetworkIdempotent(t *testing.T) {
	m := openTemp(t)
	mustCreate(t, m, "web", "10.1.0.0/24")
	if err := m.CreateNetwork("web", "10.1.0.0/24"); err != nil {
		t.Fatalf("duplicate create with same subnet must be idempotent: %v", err)
	}
	if err := m.CreateNetwork("web", "10.2.0.0/24"); err == nil {
		t.Fatal("same name with different subnet must fail")
	}
	if got := m.Networks(); len(got) != 1 || got[0] != "web" {
		t.Fatalf("Networks() = %v", got)
	}
}

func TestDeleteNetworkIdempotentAndCleansUp(t *testing.T) {
	m := openTemp(t)
	mustCreate(t, m, "web", "10.1.0.0/24")
	mustAttach(t, m, "c1", "app",
		AttachRequest{Network: "web", Ports: []PortMapping{{Proto: "tcp", HostPort: 8080, ContainerPort: 80}}})

	if err := m.DeleteNetwork("web"); err != nil {
		t.Fatalf("DeleteNetwork: %v", err)
	}
	// 重复删除幂等。
	if err := m.DeleteNetwork("web"); err != nil {
		t.Fatalf("duplicate delete must be idempotent: %v", err)
	}
	// 接入关系被清理。
	if got := m.Inspect("c1"); len(got) != 0 {
		t.Fatalf("attachments leaked after delete: %v", got)
	}
	// 端口被回收：重建网络后同端口可再发布。
	mustCreate(t, m, "web", "10.1.0.0/24")
	mustAttach(t, m, "c2", "app2",
		AttachRequest{Network: "web", Ports: []PortMapping{{Proto: "tcp", HostPort: 8080, ContainerPort: 80}}})
}

func TestAttachDetachAddressReuse(t *testing.T) {
	m := openTemp(t)
	mustCreate(t, m, "web", "10.1.0.0/24")
	mustAttach(t, m, "c1", "a", AttachRequest{Network: "web"})
	mustAttach(t, m, "c2", "b", AttachRequest{Network: "web"})
	ip1 := m.Inspect("c1")["web"].IP
	ip2 := m.Inspect("c2")["web"].IP
	if ip1 == ip2 {
		t.Fatalf("address conflict: both got %s", ip1)
	}
	// 释放 c1 后其地址可回收，c2 的地址不受影响。
	if err := m.Detach("c1"); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	mustAttach(t, m, "c3", "c", AttachRequest{Network: "web"})
	ip3 := m.Inspect("c3")["web"].IP
	if ip3 != ip1 {
		t.Fatalf("expected released address %s reused, got %s", ip1, ip3)
	}
	if got := m.Inspect("c2")["web"].IP; got != ip2 {
		t.Fatalf("in-use address changed: %s -> %s", ip2, got)
	}
	// 重复退出幂等。
	if err := m.Detach("c1"); err != nil {
		t.Fatalf("duplicate detach must be idempotent: %v", err)
	}
}

func TestAttachDuplicateNetworkRejected(t *testing.T) {
	m := openTemp(t)
	mustCreate(t, m, "web", "10.1.0.0/24")
	mustAttach(t, m, "c1", "a", AttachRequest{Network: "web"})
	if err := m.Attach("c1", "a", []AttachRequest{{Network: "web"}}); err == nil {
		t.Fatal("re-attaching to same network must fail")
	}
}

func TestMultiNetworkAttachAtomic(t *testing.T) {
	m := openTemp(t)
	mustCreate(t, m, "front", "10.1.0.0/24")
	mustCreate(t, m, "back", "10.2.0.0/24")

	// 第二个网络不存在：整体失败，front 上不得残留部分接入。
	err := m.Attach("c1", "app", []AttachRequest{
		{Network: "front"},
		{Network: "missing"},
	})
	if err == nil {
		t.Fatal("attach with missing network must fail")
	}
	if got := m.Inspect("c1"); len(got) != 0 {
		t.Fatalf("partial attach leaked: %v", got)
	}

	// 端口冲突发生在第二个网络：同样整体拒绝。
	mustAttach(t, m, "c0", "other",
		AttachRequest{Network: "front", Ports: []PortMapping{{Proto: "tcp", HostPort: 9000, ContainerPort: 90}}})
	err = m.Attach("c2", "app2", []AttachRequest{
		{Network: "front"},
		{Network: "back", Ports: []PortMapping{{Proto: "tcp", HostPort: 9000, ContainerPort: 90}}},
	})
	if err == nil || !strings.Contains(err.Error(), "9000") {
		t.Fatalf("expected port conflict error, got %v", err)
	}
	if got := m.Inspect("c2"); len(got) != 0 {
		t.Fatalf("partial attach leaked after port conflict: %v", got)
	}

	// 全部合法时一次接入两个网络，各自独立分配地址。
	mustAttach(t, m, "c3", "app3",
		AttachRequest{Network: "front"}, AttachRequest{Network: "back"})
	ins := m.Inspect("c3")
	if len(ins) != 2 {
		t.Fatalf("expected 2 attachments, got %v", ins)
	}
	if ins["front"].IP == ins["back"].IP {
		t.Fatalf("networks must allocate independently, both got %s", ins["front"].IP)
	}
}

func TestPortConflictAndLifecycle(t *testing.T) {
	m := openTemp(t)
	mustCreate(t, m, "web", "10.1.0.0/24")
	pm := PortMapping{Proto: "tcp", HostPort: 8080, ContainerPort: 80}
	mustAttach(t, m, "c1", "a", AttachRequest{Network: "web", Ports: []PortMapping{pm}})

	// 冲突必须在接入完成前被发现并拒绝。
	if err := m.Attach("c2", "b", []AttachRequest{{Network: "web", Ports: []PortMapping{pm}}}); err == nil {
		t.Fatal("port conflict must be rejected")
	}
	if got := m.Inspect("c2"); len(got) != 0 {
		t.Fatalf("rejected attach left state: %v", got)
	}
	// tcp/udp 是不同命名空间，不冲突。
	mustAttach(t, m, "c3", "c", AttachRequest{Network: "web",
		Ports: []PortMapping{{Proto: "udp", HostPort: 8080, ContainerPort: 80}}})

	// 撤销与容器生命周期一致：c1 退出后端口可再发布。
	if err := m.Detach("c1"); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	mustAttach(t, m, "c4", "d", AttachRequest{Network: "web", Ports: []PortMapping{pm}})
}

func TestResolveByNameAndAlias(t *testing.T) {
	m := openTemp(t)
	mustCreate(t, m, "web", "10.1.0.0/24")
	mustAttach(t, m, "c1", "web", AttachRequest{Network: "web", Aliases: []string{"api"}})
	mustAttach(t, m, "c2", "web", AttachRequest{Network: "web", Aliases: []string{"api"}})
	mustAttach(t, m, "c3", "db", AttachRequest{Network: "web"})

	ips, err := m.Resolve("web", "web")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(ips) != 2 {
		t.Fatalf("expected 2 replicas, got %v", ips)
	}
	alias, _ := m.Resolve("web", "api")
	if len(alias) != 2 {
		t.Fatalf("alias resolution expected 2, got %v", alias)
	}

	// 缩容：c1 下线后解析结果立即不包含其地址。
	gone := m.Inspect("c1")["web"].IP
	if err := m.Detach("c1"); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	ips, _ = m.Resolve("web", "web")
	if len(ips) != 1 || ips[0] == gone {
		t.Fatalf("stale address returned after scale-down: %v (gone=%s)", ips, gone)
	}
	// 其他网络隔离：不存在的网络报错。
	if _, err := m.Resolve("nosuch", "web"); err == nil {
		t.Fatal("resolve on missing network must fail")
	}
}
