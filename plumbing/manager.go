package plumbing

import (
	"context"
	"fmt"
	"net"
	"sort"
	"sync"
	"sync/atomic"
)

// Manager manages networks, endpoints, ports and name resolution.
// All methods are safe for concurrent use; the result of any concurrent
// combination is equivalent to some serial execution.
type Manager struct {
	mu    sync.Mutex
	store *store
	nets  map[string]*Network
	pools map[string]*ipPool
	ports *portAllocator
	txn   atomic.Uint64
}

// Open loads (or creates) the state under dir and recovers any operations
// interrupted by a crash: incomplete attaches are rolled back, incomplete
// detaches are completed.
func Open(dir string) (*Manager, error) {
	st, err := newStore(dir).loadState()
	if err != nil {
		return nil, err
	}
	m := &Manager{
		store: newStore(dir),
		nets:  map[string]*Network{},
		pools: map[string]*ipPool{},
		ports: newPortAllocator(),
	}
	for name, n := range st.Networks {
		if n.Endpoints == nil {
			n.Endpoints = map[string]Endpoint{}
		}
		m.nets[name] = n
		_, subnet, err := net.ParseCIDR(n.Subnet)
		if err != nil {
			return nil, fmt.Errorf("%w: %s", ErrInvalidSubnet, n.Subnet)
		}
		pool := newIPPool(subnet)
		for _, ep := range n.Endpoints {
			_ = pool.Reserve(ep.IP)
		}
		m.pools[name] = pool
	}
	// Rebuild port allocator from persisted map.
	for key, owner := range st.Ports {
		var p int
		var pr string
		if _, err := fmt.Sscanf(key, "%d/%s", &p, &pr); err != nil {
			continue
		}
		m.ports.used[portKey{Proto: pr, Port: p}] = owner
	}
	if err := m.recover(); err != nil {
		return nil, err
	}
	return m, nil
}

// recover replays the journal: transactions with a begin but no commit are
// finished according to their semantics (attach rolls back, detach completes).
func (m *Manager) recover() error {
	entries, err := m.store.readJournal()
	if err != nil {
		return err
	}
	type pending struct {
		begins  []journalEntry
		op      string
		commits bool
	}
	txns := map[string]*pending{}
	var order []string
	for _, e := range entries {
		t := txns[e.TxnID]
		if t == nil {
			t = &pending{}
			txns[e.TxnID] = t
			order = append(order, e.TxnID)
		}
		switch e.Op {
		case opAttachBegin, opDetachBegin:
			t.op = e.Op
			t.begins = append(t.begins, e)
		case opAttachCommit, opDetachCommit:
			t.commits = true
		}
	}
	for _, id := range order {
		t := txns[id]
		if t.commits {
			continue
		}
		for _, b := range t.begins {
			switch b.Op {
			case opAttachBegin:
				m.rollbackAttach(b)
			case opDetachBegin:
				m.applyDetach(b.Network, b.ContainerID)
			}
		}
	}
	if err := m.persistLocked(); err != nil {
		return err
	}
	return m.store.resetJournal()
}

// rollbackAttach undoes a persisted-but-uncommitted attach.
func (m *Manager) rollbackAttach(e journalEntry) {
	n, ok := m.nets[e.Network]
	if !ok {
		return
	}
	if ep, ok := n.Endpoints[e.ContainerID]; ok {
		delete(n.Endpoints, e.ContainerID)
		if pool := m.pools[e.Network]; pool != nil {
			pool.Release(ep.IP)
		}
	}
	if len(m.networksOf(e.ContainerID)) == 0 {
		m.ports.ReleaseAll(e.ContainerID)
	}
}

// networksOf returns the names of networks the container is attached to.
func (m *Manager) networksOf(containerID string) []string {
	var out []string
	for name, n := range m.nets {
		if _, ok := n.Endpoints[containerID]; ok {
			out = append(out, name)
		}
	}
	return out
}

// persistLocked writes the current state snapshot. m.mu must be held.
func (m *Manager) persistLocked() error {
	st := persistentState{
		Networks: m.nets,
		Ports:    map[string]portOwner{},
	}
	for k, o := range m.ports.used {
		st.Ports[fmt.Sprintf("%d/%s", k.Port, k.Proto)] = o
	}
	return m.store.saveState(st)
}

// CreateNetwork creates a network with the given CIDR subnet.
// Creating an existing network with the same subnet is a no-op (idempotent);
// the same name with a different subnet fails.
func (m *Manager) CreateNetwork(ctx context.Context, name, subnet string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, ipnet, err := net.ParseCIDR(subnet)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidSubnet, subnet)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.nets[name]; ok {
		if existing.Subnet == subnet {
			return nil
		}
		return fmt.Errorf("%w: %s with different subnet", ErrNetworkExists, name)
	}
	m.nets[name] = &Network{
		Name:      name,
		Subnet:    subnet,
		Endpoints: map[string]Endpoint{},
	}
	m.pools[name] = newIPPool(ipnet)
	return m.persistLocked()
}

// DeleteNetwork removes a network and every attachment on it, releasing all
// addresses and published ports. Deleting a missing network is a no-op.
func (m *Manager) DeleteNetwork(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nets[name]
	if !ok {
		return nil
	}
	for id := range n.Endpoints {
		delete(n.Endpoints, id)
		if len(m.networksOf(id)) == 0 {
			m.ports.ReleaseAll(id)
		}
	}
	delete(m.nets, name)
	delete(m.pools, name)
	return m.persistLocked()
}

// Attach connects a container to one network, allocating an address and
// publishing its ports. The operation is atomic: on any failure nothing is
// left behind.
func (m *Manager) Attach(ctx context.Context, network string, req AttachRequest) (*Endpoint, error) {
	eps, err := m.AttachAll(ctx, []string{network}, req)
	if err != nil {
		return nil, err
	}
	return &eps[0], nil
}

// AttachAll connects a container to several networks atomically: either all
// attachments succeed or none remain. One journal transaction covers the
// whole batch, so a crash mid-way is fully rolled back on recovery.
func (m *Manager) AttachAll(ctx context.Context, networks []string, req AttachRequest) ([]Endpoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.ContainerID == "" {
		return nil, fmt.Errorf("%w: empty container id", ErrInvalidPortMap)
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, nw := range networks {
		n, ok := m.nets[nw]
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrNetworkNotFound, nw)
		}
		if _, ok := n.Endpoints[req.ContainerID]; ok {
			return nil, fmt.Errorf("%w: %s on %s", ErrContainerExists, req.ContainerID, nw)
		}
	}

	// Pre-validate ports before any state change so conflicts are detected
	// before the attach completes. Ports are published once per container;
	// if the container already has published ports, additional attaches do
	// not re-reserve them.
	needPorts := len(m.ports.Of(req.ContainerID)) == 0 && len(req.Ports) > 0
	if needPorts {
		if err := m.ports.Reserve(req.ContainerID, req.Ports); err != nil {
			return nil, err
		}
	}

	txnID := fmt.Sprintf("txn-%d", m.txn.Add(1))
	var eps []Endpoint
	allocated := make(map[string]string) // network -> ip, for rollback
	fail := func(cause error) ([]Endpoint, error) {
		for nw, ip := range allocated {
			delete(m.nets[nw].Endpoints, req.ContainerID)
			m.pools[nw].Release(ip)
		}
		if needPorts {
			m.ports.ReleaseAll(req.ContainerID)
		}
		return nil, cause
	}

	for _, nw := range networks {
		ip, err := m.pools[nw].Allocate()
		if err != nil {
			return fail(err)
		}
		ep := Endpoint{
			ContainerID: req.ContainerID,
			Name:        req.Name,
			Aliases:     req.Aliases,
			IP:          ip,
		}
		// Record intent before mutating persistent state.
		if err := m.store.appendJournal(journalEntry{
			Op:          opAttachBegin,
			TxnID:       txnID,
			Network:     nw,
			ContainerID: req.ContainerID,
			Endpoint:    &ep,
			Ports:       req.Ports,
		}); err != nil {
			return fail(err)
		}
		m.nets[nw].Endpoints[req.ContainerID] = ep
		allocated[nw] = ip
		eps = append(eps, ep)
	}
	if err := m.persistLocked(); err != nil {
		return fail(err)
	}
	if err := m.store.appendJournal(journalEntry{Op: opAttachCommit, TxnID: txnID}); err != nil {
		return nil, err // state is persisted; recovery sees no begin-without-commit hazard
	}
	if err := m.store.resetJournal(); err != nil {
		return nil, err
	}
	return eps, nil
}

// applyDetach removes an endpoint and releases its resources. Idempotent.
func (m *Manager) applyDetach(network, containerID string) {
	n, ok := m.nets[network]
	if !ok {
		return
	}
	ep, ok := n.Endpoints[containerID]
	if !ok {
		return
	}
	delete(n.Endpoints, containerID)
	if pool := m.pools[network]; pool != nil {
		pool.Release(ep.IP)
	}
	if len(m.networksOf(containerID)) == 0 {
		m.ports.ReleaseAll(containerID)
	}
}

// Detach removes a container from a network, releasing its address and, when
// the container leaves its last network, its published ports. Detaching a
// container that is not attached is a no-op.
func (m *Manager) Detach(ctx context.Context, network, containerID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.nets[network]; !ok {
		return fmt.Errorf("%w: %s", ErrNetworkNotFound, network)
	}
	txnID := fmt.Sprintf("txn-%d", m.txn.Add(1))
	if err := m.store.appendJournal(journalEntry{
		Op:          opDetachBegin,
		TxnID:       txnID,
		Network:     network,
		ContainerID: containerID,
	}); err != nil {
		return err
	}
	m.applyDetach(network, containerID)
	if err := m.persistLocked(); err != nil {
		return err
	}
	if err := m.store.appendJournal(journalEntry{Op: opDetachCommit, TxnID: txnID}); err != nil {
		return err
	}
	return m.store.resetJournal()
}

// Resolve returns the sorted live addresses for a container name or alias
// within a network. Only currently attached endpoints are returned.
func (m *Manager) Resolve(ctx context.Context, network, nameOrAlias string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nets[network]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNetworkNotFound, network)
	}
	var out []string
	for _, ep := range n.Endpoints {
		if ep.Name == nameOrAlias {
			out = append(out, ep.IP)
			continue
		}
		for _, a := range ep.Aliases {
			if a == nameOrAlias {
				out = append(out, ep.IP)
				break
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// Network returns a copy of the named network's state.
func (m *Manager) Network(ctx context.Context, name string) (*Network, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nets[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNetworkNotFound, name)
	}
	cp := &Network{
		Name:      n.Name,
		Subnet:    n.Subnet,
		Endpoints: make(map[string]Endpoint, len(n.Endpoints)),
	}
	for k, v := range n.Endpoints {
		cp.Endpoints[k] = v
	}
	return cp, nil
}

// PortsOf returns the published host ports of a container.
func (m *Manager) PortsOf(ctx context.Context, containerID string) ([]PortMapping, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ports.Of(containerID), nil
}
