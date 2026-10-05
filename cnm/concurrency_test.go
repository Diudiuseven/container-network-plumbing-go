package cnm

import (
	"fmt"
	"sync"
	"testing"
)

// TestConcurrentAttachUniqueResources 并发接入：地址与端口不得重复分配。
func TestConcurrentAttachUniqueResources(t *testing.T) {
	m := openTemp(t)
	mustCreate(t, m, "web", "10.1.0.0/24")

	const n = 50
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = m.Attach(fmt.Sprintf("c%d", i), fmt.Sprintf("svc-%d", i%5),
				[]AttachRequest{{Network: "web",
					Ports: []PortMapping{{Proto: "tcp", HostPort: 10000 + i, ContainerPort: 80}}}})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("attach c%d: %v", i, err)
		}
	}

	// 地址唯一且数量正确。
	seen := map[string]string{}
	for i := 0; i < n; i++ {
		ins := m.Inspect(fmt.Sprintf("c%d", i))
		att, ok := ins["web"]
		if !ok {
			t.Fatalf("c%d not attached", i)
		}
		if owner, dup := seen[att.IP]; dup {
			t.Fatalf("ip %s assigned to both %s and c%d", att.IP, owner, i)
		}
		seen[att.IP] = fmt.Sprintf("c%d", i)
	}
	// 解析结果与串行预期一致：5 个服务名各 10 个副本。
	for s := 0; s < 5; s++ {
		ips, err := m.Resolve("web", fmt.Sprintf("svc-%d", s))
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if len(ips) != 10 {
			t.Fatalf("svc-%d: expected 10 replicas, got %d", s, len(ips))
		}
	}
}

// TestConcurrentPortConflict 并发发布同一端口：恰好一个成功。
func TestConcurrentPortConflict(t *testing.T) {
	m := openTemp(t)
	mustCreate(t, m, "web", "10.1.0.0/24")

	const n = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	succeeded := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := m.Attach(fmt.Sprintf("c%d", i), "svc",
				[]AttachRequest{{Network: "web",
					Ports: []PortMapping{{Proto: "tcp", HostPort: 8080, ContainerPort: 80}}}})
			mu.Lock()
			if err == nil {
				succeeded++
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if succeeded != 1 {
		t.Fatalf("expected exactly 1 winner for port 8080, got %d", succeeded)
	}
}

// TestConcurrentMixedOpsConverge 并发接入/退出/查询混合执行：
// 最终状态必须内部一致（等价于某种串行执行的结果）。
func TestConcurrentMixedOpsConverge(t *testing.T) {
	m := openTemp(t)
	mustCreate(t, m, "front", "10.1.0.0/24")
	mustCreate(t, m, "back", "10.2.0.0/24")

	const n = 30
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("c%d", i)
			reqs := []AttachRequest{{Network: "front"}, {Network: "back"}}
			if err := m.Attach(id, "svc", reqs); err != nil {
				t.Errorf("attach %s: %v", id, err)
				return
			}
			// 查询不得看到中间态：要么两个网络都在，要么都不在（对自身而言
			// attach 返回后必须两个都在）。
			ins := m.Inspect(id)
			if len(ins) != 0 && len(ins) != 2 {
				t.Errorf("observed partial attach of %s: %v", id, ins)
			}
			if i%2 == 0 {
				if err := m.Detach(id, "front"); err != nil {
					t.Errorf("detach %s: %v", id, err)
				}
			} else {
				if err := m.Detach(id); err != nil {
					t.Errorf("detach-all %s: %v", id, err)
				}
			}
			if _, err := m.Resolve("front", "svc"); err != nil {
				t.Errorf("resolve: %v", err)
			}
		}(i)
	}
	wg.Wait()

	// 收敛性校验：最终状态必须等价于串行执行——
	// 偶数容器只在 back，奇数容器完全退出。
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("c%d", i)
		ins := m.Inspect(id)
		if i%2 == 0 {
			if len(ins) != 1 {
				t.Fatalf("%s: expected only back attachment, got %v", id, ins)
			}
			if _, ok := ins["back"]; !ok {
				t.Fatalf("%s: expected back attachment, got %v", id, ins)
			}
		} else if len(ins) != 0 {
			t.Fatalf("%s: expected no attachments, got %v", id, ins)
		}
	}
	// 地址分配与接入关系一致：每个已分配地址都属于在线容器。
	st := m.st
	for netName, n := range st.Networks {
		for ip, owner := range n.Allocations {
			att, ok := n.Containers[owner]
			if !ok {
				t.Fatalf("net %s: ip %s allocated to detached container %s", netName, ip, owner)
			}
			if att.IP != ip {
				t.Fatalf("net %s: allocation %s->%s mismatches attachment ip %s", netName, ip, owner, att.IP)
			}
		}
	}
	// 无端口残留。
	if len(st.Ports) != 0 {
		t.Fatalf("port leaks: %v", st.Ports)
	}
	// 无遗留日志。
	if st.Pending != nil {
		t.Fatalf("pending txn leaked: %+v", st.Pending)
	}
}

// TestConcurrentCreateDeleteIdempotent 并发重复创建/删除同一网络。
func TestConcurrentCreateDeleteIdempotent(t *testing.T) {
	m := openTemp(t)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _ = m.CreateNetwork("web", "10.1.0.0/24") }()
		go func() { defer wg.Done(); _ = m.DeleteNetwork("gone") }()
	}
	wg.Wait()
	if got := m.Networks(); len(got) != 1 || got[0] != "web" {
		t.Fatalf("Networks() = %v", got)
	}
}
