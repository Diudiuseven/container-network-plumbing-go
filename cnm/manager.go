package cnm

import (
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Manager 是容器网络管理器。
//
// 并发语义：所有变更在内部全局锁下串行提交，查询使用读锁，
// 因此任意并发组合的最终状态与某种串行执行一致，
// 且查询永远只能看到已提交（已落盘）的状态。
type Manager struct {
	mu    sync.RWMutex
	st    *state
	store *store

	// failpoint 仅供测试注入“进程被强杀”：在指定阶段 panic，
	// 模拟崩溃后调用方应丢弃该 Manager 并重新 Open。
	failpoint func(phase string)
}

// Open 加载（或初始化）位于 dir 的状态，并对未完成的变更执行崩溃恢复：
//   - 未完成的 attach：回滚，释放 prepare 阶段预留的地址与端口；
//   - 未完成的 detach：完成，继续清理接入关系、地址与端口。
func Open(dir string) (*Manager, error) {
	st, err := newStore(filepath.Join(dir, "state.json")).load()
	if err != nil {
		return nil, err
	}
	m := &Manager{st: st, store: newStore(filepath.Join(dir, "state.json"))}
	if err := m.recover(); err != nil {
		return nil, fmt.Errorf("recover: %w", err)
	}
	return m, nil
}

// CreateNetwork 创建网络。重复创建同名同子网网络是幂等的；
// 同名不同子网返回错误。
func (m *Manager) CreateNetwork(name, subnet string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n, ok := m.st.Networks[name]; ok {
		if n.Subnet == subnet {
			return nil // 幂等
		}
		return fmt.Errorf("network %q already exists with subnet %s", name, n.Subnet)
	}
	p, err := newPool(subnet)
	if err != nil {
		return err
	}
	m.st.Networks[name] = &Network{
		Name:        name,
		Subnet:      subnet,
		Gateway:     p.gateway(),
		Allocations: map[string]string{},
		Containers:  map[string]*Attachment{},
	}
	return m.store.save(m.st)
}

// DeleteNetwork 删除网络，并完整清理其上的接入关系、地址分配与端口发布。
// 删除不存在的网络是幂等成功。
func (m *Manager) DeleteNetwork(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.st.Networks[name]
	if !ok {
		return nil // 幂等
	}
	for _, att := range n.Containers {
		m.releasePorts(att)
	}
	delete(m.st.Networks, name)
	return m.store.save(m.st)
}

// Attach 把容器原子地接入一个或多个网络。
//
// 流程分为 prepare 与 commit 两个阶段：
//  1. prepare：校验（网络存在、未重复接入、端口无冲突），分配地址并预留
//     端口，写入日志（journal）后落盘；
//  2. commit：接入关系变为可见，清除日志后落盘。
//
// 任一网络校验失败时整体拒绝，不会留下部分接入状态；
// 进程在两阶段之间崩溃时，重启后回滚 prepare 预留的资源。
func (m *Manager) Attach(containerID, name string, reqs []AttachRequest) error {
	if containerID == "" {
		return fmt.Errorf("container id must not be empty")
	}
	if len(reqs) == 0 {
		return fmt.Errorf("at least one network is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	// ---- 校验阶段：任何失败都不改变状态 ----
	seen := map[string]bool{}
	for _, r := range reqs {
		if seen[r.Network] {
			return fmt.Errorf("duplicate network %q in attach request", r.Network)
		}
		seen[r.Network] = true
		n, ok := m.st.Networks[r.Network]
		if !ok {
			return fmt.Errorf("network %q does not exist", r.Network)
		}
		if _, attached := n.Containers[containerID]; attached {
			return fmt.Errorf("container %s already attached to network %q", containerID, r.Network)
		}
	}
	// 端口冲突必须在接入完成前被发现：先对全部请求做占用检查。
	reqPorts := map[PortKey]bool{}
	for _, r := range reqs {
		for _, pm := range r.Ports {
			if err := validatePortMapping(pm); err != nil {
				return err
			}
			k := PortKey{Proto: pm.Proto, Port: pm.HostPort}
			if reqPorts[k] {
				return fmt.Errorf("port %s/%d published twice in one request", k.Proto, k.Port)
			}
			reqPorts[k] = true
			if owner, taken := m.st.Ports[k]; taken {
				return fmt.Errorf("host port %s/%d already published by container %s", k.Proto, k.Port, owner)
			}
		}
	}

	// ---- prepare：分配地址、预留端口、写日志并落盘 ----
	txn := &Txn{Op: "attach", ContainerID: containerID, Name: name}
	ips := map[string]string{}
	for _, r := range reqs {
		n := m.st.Networks[r.Network]
		p, err := newPool(n.Subnet)
		if err != nil {
			return err
		}
		ip, err := p.allocate(n.Allocations)
		if err != nil {
			return fmt.Errorf("network %q: %w", r.Network, err)
		}
		ips[r.Network] = ip
		n.Allocations[ip] = containerID
		txn.Reserved = append(txn.Reserved, txnReservation{Network: r.Network, IP: ip})
		txn.Requests = append(txn.Requests, txnAttach{Network: r.Network, Aliases: r.Aliases, Ports: r.Ports})
		for _, pm := range r.Ports {
			m.st.Ports[PortKey{Proto: pm.Proto, Port: pm.HostPort}] = containerID
		}
	}
	m.st.Pending = txn
	if err := m.store.save(m.st); err != nil {
		// 落盘失败：回滚内存中的预留，保持内存与磁盘一致。
		m.rollbackAttachLocked(txn)
		return err
	}
	if m.failpoint != nil {
		m.failpoint("attach:prepared")
	}

	// ---- commit：接入关系可见，清除日志并落盘 ----
	now := time.Now().UTC()
	for _, ta := range txn.Requests {
		n := m.st.Networks[ta.Network]
		n.Containers[containerID] = &Attachment{
			ContainerID: containerID,
			Name:        name,
			Aliases:     append([]string(nil), ta.Aliases...),
			IP:          ips[ta.Network],
			Ports:       append([]PortMapping(nil), ta.Ports...),
			JoinedAt:    now,
		}
	}
	m.st.Pending = nil
	return m.store.save(m.st)
}

// Detach 把容器从指定网络退出，回收地址并撤销端口发布。
// networks 为空表示退出全部网络；退出未接入的网络/容器是幂等成功。
func (m *Manager) Detach(containerID string, networks ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	targets := m.detachTargetsLocked(containerID, networks)
	if len(targets) == 0 {
		return nil // 幂等：没有需要退出的接入
	}

	// ---- prepare：写日志并落盘 ----
	m.st.Pending = &Txn{Op: "detach", ContainerID: containerID, DetachNetworks: targets}
	if err := m.store.save(m.st); err != nil {
		return err
	}
	if m.failpoint != nil {
		m.failpoint("detach:prepared")
	}

	// ---- commit：执行清理，清除日志并落盘 ----
	m.applyDetachLocked(containerID, targets)
	m.st.Pending = nil
	return m.store.save(m.st)
}

// Resolve 在网络内按容器名或别名解析，返回当前在线容器的地址（升序、去重）。
// 结果只来自已提交状态，因此不会返回已下线容器的地址。
func (m *Manager) Resolve(network, nameOrAlias string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n, ok := m.st.Networks[network]
	if !ok {
		return nil, fmt.Errorf("network %q does not exist", network)
	}
	var ips []string
	for _, att := range n.Containers {
		if att.Name == nameOrAlias || contains(att.Aliases, nameOrAlias) {
			ips = append(ips, att.IP)
		}
	}
	sort.Strings(ips)
	return ips, nil
}

// Inspect 返回容器当前的接入快照（按网络名）。
func (m *Manager) Inspect(containerID string) map[string]Attachment {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[string]Attachment{}
	for netName, n := range m.st.Networks {
		if att, ok := n.Containers[containerID]; ok {
			out[netName] = *att
		}
	}
	return out
}

// Networks 返回当前存在的网络名列表（升序）。
func (m *Manager) Networks() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.st.Networks))
	for name := range m.st.Networks {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ---- 内部辅助（均需已持有写锁，除注明外） ----

// recover 在 Open 时处理未完成的变更。
func (m *Manager) recover() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	txn := m.st.Pending
	if txn == nil {
		return nil
	}
	switch txn.Op {
	case "attach":
		// 回滚：prepare 预留的地址与端口全部释放，接入关系从未可见。
		m.rollbackAttachLocked(txn)
	case "detach":
		// 完成：继续执行清理（幂等）。
		m.applyDetachLocked(txn.ContainerID, txn.DetachNetworks)
	default:
		return fmt.Errorf("unknown pending txn op %q", txn.Op)
	}
	m.st.Pending = nil
	return m.store.save(m.st)
}

// rollbackAttachLocked 回滚一次未完成 attach 的 prepare 预留：
// 释放地址与端口，并防御性地移除可能已提交的接入关系。幂等。
func (m *Manager) rollbackAttachLocked(txn *Txn) {
	for _, res := range txn.Reserved {
		if n, ok := m.st.Networks[res.Network]; ok {
			if owner, ok := n.Allocations[res.IP]; ok && owner == txn.ContainerID {
				delete(n.Allocations, res.IP)
			}
		}
	}
	for _, ta := range txn.Requests {
		for _, pm := range ta.Ports {
			k := PortKey{Proto: pm.Proto, Port: pm.HostPort}
			if owner, ok := m.st.Ports[k]; ok && owner == txn.ContainerID {
				delete(m.st.Ports, k)
			}
		}
		// 防御：若崩溃发生在 commit 落盘之后、日志清除之前（理论上
		// 单文件原子写不会发生），确保接入关系也被移除。
		if n, ok := m.st.Networks[ta.Network]; ok {
			delete(n.Containers, txn.ContainerID)
		}
	}
}

// detachTargetsLocked 计算实际需要退出的网络（容器已接入且被指定）。
func (m *Manager) detachTargetsLocked(containerID string, networks []string) []string {
	want := map[string]bool{}
	for _, name := range networks {
		want[name] = true
	}
	all := len(networks) == 0
	var targets []string
	for netName, n := range m.st.Networks {
		if _, ok := n.Containers[containerID]; !ok {
			continue
		}
		if all || want[netName] {
			targets = append(targets, netName)
		}
	}
	sort.Strings(targets)
	return targets
}

// applyDetachLocked 移除容器在指定网络上的接入关系，回收地址与端口。幂等。
func (m *Manager) applyDetachLocked(containerID string, networks []string) {
	for _, netName := range networks {
		n, ok := m.st.Networks[netName]
		if !ok {
			continue
		}
		att, ok := n.Containers[containerID]
		if !ok {
			continue
		}
		m.releasePorts(att)
		delete(n.Allocations, att.IP)
		delete(n.Containers, containerID)
	}
}

// releasePorts 撤销一条接入关系发布的全部宿主机端口。
func (m *Manager) releasePorts(att *Attachment) {
	for _, pm := range att.Ports {
		k := PortKey{Proto: pm.Proto, Port: pm.HostPort}
		if owner, ok := m.st.Ports[k]; ok && owner == att.ContainerID {
			delete(m.st.Ports, k)
		}
	}
}

func validatePortMapping(pm PortMapping) error {
	if pm.Proto != "tcp" && pm.Proto != "udp" {
		return fmt.Errorf("invalid protocol %q: must be tcp or udp", pm.Proto)
	}
	if pm.HostPort < 1 || pm.HostPort > 65535 {
		return fmt.Errorf("invalid host port %d", pm.HostPort)
	}
	if pm.ContainerPort < 1 || pm.ContainerPort > 65535 {
		return fmt.Errorf("invalid container port %d", pm.ContainerPort)
	}
	return nil
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
