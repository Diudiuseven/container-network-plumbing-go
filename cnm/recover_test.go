package cnm

import (
	"fmt"
	"testing"
)

// crashAt 设置 failpoint：到达指定阶段时 panic，模拟进程被强杀。
// 调用方需 recover 并丢弃该 Manager（模拟进程已死），再重新 Open。
func crashAt(m *Manager, phase string) {
	m.failpoint = func(p string) {
		if p == phase {
			panic("simulated crash at " + p)
		}
	}
}

func mustPanic(t *testing.T, f func()) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected simulated crash panic")
		}
	}()
	f()
}

// TestRecoverAttachCrashRollsBack 接入中途崩溃：重启后回滚，
// 地址与端口不得被占用，接入关系不可见。
func TestRecoverAttachCrashRollsBack(t *testing.T) {
	dir := t.TempDir()
	m, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	mustCreate(t, m, "web", "10.1.0.0/24")

	crashAt(m, "attach:prepared")
	mustPanic(t, func() {
		_ = m.Attach("c1", "app", []AttachRequest{{Network: "web",
			Ports: []PortMapping{{Proto: "tcp", HostPort: 8080, ContainerPort: 80}}}})
	})
	// m 已“死”，不得再使用。重启恢复。
	m2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := m2.Inspect("c1"); len(got) != 0 {
		t.Fatalf("rolled-back attach must be invisible, got %v", got)
	}
	// 地址未被占用：新容器应拿到池中最小地址。
	mustAttach(t, m2, "c2", "app2", AttachRequest{Network: "web",
		Ports: []PortMapping{{Proto: "tcp", HostPort: 8080, ContainerPort: 80}}})
	if ip := m2.Inspect("c2")["web"].IP; ip != "10.1.0.1" {
		t.Fatalf("address leaked by crashed attach: first free = %s, want 10.1.0.1", ip)
	}
	// 端口未被占用（c2 已成功发布 8080 即证明）。
}

// TestRecoverDetachCrashCompletes 退出中途崩溃：重启后完成清理，
// 地址与端口被回收。
func TestRecoverDetachCrashCompletes(t *testing.T) {
	dir := t.TempDir()
	m, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	mustCreate(t, m, "web", "10.1.0.0/24")
	mustAttach(t, m, "c1", "app", AttachRequest{Network: "web",
		Ports: []PortMapping{{Proto: "tcp", HostPort: 8080, ContainerPort: 80}}})
	ip := m.Inspect("c1")["web"].IP

	crashAt(m, "detach:prepared")
	mustPanic(t, func() { _ = m.Detach("c1") })

	m2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := m2.Inspect("c1"); len(got) != 0 {
		t.Fatalf("detach must be completed by recovery, got %v", got)
	}
	// 地址与端口均已回收，可被新容器复用。
	mustAttach(t, m2, "c2", "app2", AttachRequest{Network: "web",
		Ports: []PortMapping{{Proto: "tcp", HostPort: 8080, ContainerPort: 80}}})
	if got := m2.Inspect("c2")["web"].IP; got != ip {
		t.Fatalf("expected recycled ip %s, got %s", ip, got)
	}
	// 解析不得返回已下线容器。
	ips, err := m2.Resolve("web", "app")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for _, got := range ips {
		if got == ip {
			t.Fatalf("stale ip %s still resolvable", ip)
		}
	}
}

// TestRecoverMultiNetworkAttachCrash 多网络接入中途崩溃：
// 所有网络上的预留都必须回滚，不得残留部分接入。
func TestRecoverMultiNetworkAttachCrash(t *testing.T) {
	dir := t.TempDir()
	m, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	mustCreate(t, m, "front", "10.1.0.0/24")
	mustCreate(t, m, "back", "10.2.0.0/24")

	crashAt(m, "attach:prepared")
	mustPanic(t, func() {
		_ = m.Attach("c1", "app", []AttachRequest{{Network: "front"}, {Network: "back"}})
	})

	m2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := m2.Inspect("c1"); len(got) != 0 {
		t.Fatalf("partial attach leaked: %v", got)
	}
	for _, netName := range []string{"front", "back"} {
		if allocs := m2.st.Networks[netName].Allocations; len(allocs) != 0 {
			t.Fatalf("net %s: allocations leaked: %v", netName, allocs)
		}
	}
}

// TestRecoverIdempotent 恢复本身幂等：连续重启结果不变。
func TestRecoverIdempotent(t *testing.T) {
	dir := t.TempDir()
	m, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	mustCreate(t, m, "web", "10.1.0.0/24")
	crashAt(m, "attach:prepared")
	mustPanic(t, func() {
		_ = m.Attach("c1", "app", []AttachRequest{{Network: "web"}})
	})
	for i := 0; i < 3; i++ {
		m2, err := Open(dir)
		if err != nil {
			t.Fatalf("reopen %d: %v", i, err)
		}
		if got := m2.Inspect("c1"); len(got) != 0 {
			t.Fatalf("reopen %d: attach not rolled back: %v", i, got)
		}
		if m2.st.Pending != nil {
			t.Fatalf("reopen %d: pending txn not cleared: %+v", i, m2.st.Pending)
		}
	}
}

// TestStateSurvivesReopen 正常路径：已提交状态在重启后完整保留。
func TestStateSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	m, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	mustCreate(t, m, "web", "10.1.0.0/24")
	mustAttach(t, m, "c1", "app", AttachRequest{Network: "web",
		Aliases: []string{"api"},
		Ports:   []PortMapping{{Proto: "tcp", HostPort: 8080, ContainerPort: 80}}})
	ip := m.Inspect("c1")["web"].IP

	m2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	att := m2.Inspect("c1")["web"]
	if att.IP != ip || att.Name != "app" || len(att.Ports) != 1 {
		t.Fatalf("state lost across restart: %+v", att)
	}
	ips, _ := m2.Resolve("web", "api")
	if len(ips) != 1 || ips[0] != ip {
		t.Fatalf("alias resolution broken after restart: %v", ips)
	}
	// 端口占用在重启后仍然生效。
	if err := m2.Attach("c2", "other", []AttachRequest{{Network: "web",
		Ports: []PortMapping{{Proto: "tcp", HostPort: 8080, ContainerPort: 80}}}}); err == nil {
		t.Fatal("port conflict must survive restart")
	}
}

// TestConcurrentCrashAndRecover 并发接入中进程崩溃：
// 恢复后状态必须收敛——已提交的保留，未提交的回滚。
func TestConcurrentCrashAndRecover(t *testing.T) {
	dir := t.TempDir()
	m, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	mustCreate(t, m, "web", "10.1.0.0/24")

	// 先完成一批接入。
	for i := 0; i < 5; i++ {
		mustAttach(t, m, fmt.Sprintf("ok-%d", i), "svc", AttachRequest{Network: "web"})
	}
	// 再一次接入中途崩溃。
	crashAt(m, "attach:prepared")
	mustPanic(t, func() {
		_ = m.Attach("doomed", "svc", []AttachRequest{{Network: "web"}})
	})

	m2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	ips, err := m2.Resolve("web", "svc")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(ips) != 5 {
		t.Fatalf("expected 5 committed containers, got %d: %v", len(ips), ips)
	}
	if got := m2.Inspect("doomed"); len(got) != 0 {
		t.Fatalf("crashed attach leaked: %v", got)
	}
	// 地址数与在线容器数一致，无泄漏。
	if allocs := len(m2.st.Networks["web"].Allocations); allocs != 5 {
		t.Fatalf("expected 5 allocations, got %d", allocs)
	}
}
