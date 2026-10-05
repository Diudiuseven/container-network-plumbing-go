package plumbing

import (
	"context"
	"errors"
	"testing"
)

// reopen simulates a process restart on the same state directory.
func reopen(t *testing.T, dir string) *Manager {
	t.Helper()
	m, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	return m
}

func TestStateSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	m, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = m.CreateNetwork(ctx, "web", "10.1.0.0/24")
	ep, err := m.Attach(ctx, "web", AttachRequest{
		ContainerID: "c1", Name: "app", Aliases: []string{"web"},
		Ports: []PortMapping{{HostPort: 8080, ContainerPort: 80, Proto: "tcp"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	m2 := reopen(t, dir)
	ips, err := m2.Resolve(ctx, "web", "app")
	if err != nil || len(ips) != 1 || ips[0] != ep.IP {
		t.Fatalf("state lost across restart: %v %v", ips, err)
	}
	ports, _ := m2.PortsOf(ctx, "c1")
	if len(ports) != 1 || ports[0].HostPort != 8080 {
		t.Fatalf("ports lost across restart: %v", ports)
	}
	// The same address must not be handed out again after restart.
	ep2, err := m2.Attach(ctx, "web", AttachRequest{ContainerID: "c2"})
	if err != nil {
		t.Fatal(err)
	}
	if ep2.IP == ep.IP {
		t.Fatalf("address %s double-allocated after restart", ep.IP)
	}
	// The same port must still conflict after restart.
	_, err = m2.Attach(ctx, "web", AttachRequest{
		ContainerID: "c3",
		Ports:       []PortMapping{{HostPort: 8080, ContainerPort: 80, Proto: "tcp"}},
	})
	if !errors.Is(err, ErrPortInUse) {
		t.Fatalf("port conflict not detected after restart: %v", err)
	}
}

// TestRecoveryRollsBackInterruptedAttach simulates a crash after the attach
// state was persisted but before the transaction committed: recovery must
// roll the attach back, freeing the address and ports.
func TestRecoveryRollsBackInterruptedAttach(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	m, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = m.CreateNetwork(ctx, "web", "10.1.0.0/24")
	victim, err := m.Attach(ctx, "web", AttachRequest{
		ContainerID: "victim", Name: "app",
		Ports: []PortMapping{{HostPort: 9000, ContainerPort: 80, Proto: "tcp"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Craft a journal as if the process died right after persisting the
	// attach but before writing the commit record.
	s := newStore(dir)
	if err := s.appendJournal(journalEntry{
		Op:          opAttachBegin,
		TxnID:       "txn-crash",
		Network:     "web",
		ContainerID: "victim",
		Endpoint:    victim,
		Ports:       []PortMapping{{HostPort: 9000, ContainerPort: 80, Proto: "tcp"}},
	}); err != nil {
		t.Fatal(err)
	}

	m2 := reopen(t, dir)
	n, _ := m2.Network(ctx, "web")
	if _, ok := n.Endpoints["victim"]; ok {
		t.Fatal("interrupted attach must be rolled back")
	}
	if ports, _ := m2.PortsOf(ctx, "victim"); len(ports) != 0 {
		t.Fatalf("interrupted attach leaked ports: %v", ports)
	}
	// Both the address and the port must be reusable immediately.
	ep, err := m2.Attach(ctx, "web", AttachRequest{
		ContainerID: "new", Name: "app",
		Ports: []PortMapping{{HostPort: 9000, ContainerPort: 80, Proto: "tcp"}},
	})
	if err != nil {
		t.Fatalf("resources of rolled-back attach must be reusable: %v", err)
	}
	if ep.IP != victim.IP {
		t.Fatalf("expected rolled-back address %s to be reused, got %s", victim.IP, ep.IP)
	}
}

// TestRecoveryCompletesInterruptedDetach simulates a crash after a detach
// began: recovery must finish the detach so no address or port leaks.
func TestRecoveryCompletesInterruptedDetach(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	m, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = m.CreateNetwork(ctx, "web", "10.1.0.0/24")
	gone, err := m.Attach(ctx, "web", AttachRequest{
		ContainerID: "gone", Name: "app",
		Ports: []PortMapping{{HostPort: 9001, ContainerPort: 80, Proto: "tcp"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Crash right after the detach intent was written, before the state
	// snapshot was updated: the endpoint is still in state.json.
	s := newStore(dir)
	if err := s.appendJournal(journalEntry{
		Op:          opDetachBegin,
		TxnID:       "txn-crash",
		Network:     "web",
		ContainerID: "gone",
	}); err != nil {
		t.Fatal(err)
	}

	m2 := reopen(t, dir)
	n, _ := m2.Network(ctx, "web")
	if _, ok := n.Endpoints["gone"]; ok {
		t.Fatal("interrupted detach must be completed during recovery")
	}
	if ips, _ := m2.Resolve(ctx, "web", "app"); len(ips) != 0 {
		t.Fatalf("detached container still resolves: %v", ips)
	}
	ep, err := m2.Attach(ctx, "web", AttachRequest{
		ContainerID: "new",
		Ports:       []PortMapping{{HostPort: 9001, ContainerPort: 80, Proto: "tcp"}},
	})
	if err != nil {
		t.Fatalf("resources of completed detach must be reusable: %v", err)
	}
	if ep.IP != gone.IP {
		t.Fatalf("expected detached address %s to be reused, got %s", gone.IP, ep.IP)
	}
}

// TestRecoveryIgnoresCommittedTransactions ensures a committed transaction
// left in the journal (crash before truncation) is not rolled back.
func TestRecoveryIgnoresCommittedTransactions(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	m, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = m.CreateNetwork(ctx, "web", "10.1.0.0/24")
	ep, err := m.Attach(ctx, "web", AttachRequest{ContainerID: "c1", Name: "app"})
	if err != nil {
		t.Fatal(err)
	}
	s := newStore(dir)
	_ = s.appendJournal(journalEntry{
		Op: opAttachBegin, TxnID: "txn-ok", Network: "web",
		ContainerID: "c1", Endpoint: ep,
	})
	_ = s.appendJournal(journalEntry{Op: opAttachCommit, TxnID: "txn-ok"})

	m2 := reopen(t, dir)
	ips, _ := m2.Resolve(ctx, "web", "app")
	if len(ips) != 1 || ips[0] != ep.IP {
		t.Fatalf("committed attach must survive recovery: %v", ips)
	}
}

// TestRecoveryIgnoresTornJournalLine simulates a crash in the middle of
// writing a journal record: the partial line must be ignored.
func TestRecoveryIgnoresTornJournalLine(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	m, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = m.CreateNetwork(ctx, "web", "10.1.0.0/24")
	if _, err := m.Attach(ctx, "web", AttachRequest{ContainerID: "c1", Name: "app"}); err != nil {
		t.Fatal(err)
	}
	// Append a half-written JSON record.
	s := newStore(dir)
	f, err := os_OpenFileAppend(s.journalPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"op":"attach-begin","txnId":"txn-torn","netwo`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	m2 := reopen(t, dir)
	ips, _ := m2.Resolve(ctx, "web", "app")
	if len(ips) != 1 {
		t.Fatalf("torn journal line must not affect recovery: %v", ips)
	}
}

// TestRecoveryRollsBackMultiNetworkAttach simulates a crash in the middle of
// an atomic multi-network attach: every network of the transaction must be
// rolled back, leaving no partial attachment.
func TestRecoveryRollsBackMultiNetworkAttach(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	m, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = m.CreateNetwork(ctx, "a", "10.1.0.0/24")
	_ = m.CreateNetwork(ctx, "b", "10.2.0.0/24")
	eps, err := m.AttachAll(ctx, []string{"a", "b"}, AttachRequest{
		ContainerID: "victim", Name: "app",
		Ports: []PortMapping{{HostPort: 9100, ContainerPort: 80, Proto: "tcp"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Crash after both attach intents were journaled and persisted, but
	// before the shared commit record was written.
	s := newStore(dir)
	for i, nw := range []string{"a", "b"} {
		ep := eps[i]
		if err := s.appendJournal(journalEntry{
			Op: opAttachBegin, TxnID: "txn-multi", Network: nw,
			ContainerID: "victim", Endpoint: &ep,
			Ports: []PortMapping{{HostPort: 9100, ContainerPort: 80, Proto: "tcp"}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	m2 := reopen(t, dir)
	for _, nw := range []string{"a", "b"} {
		n, _ := m2.Network(ctx, nw)
		if _, ok := n.Endpoints["victim"]; ok {
			t.Fatalf("network %s kept a partially attached container", nw)
		}
	}
	if ports, _ := m2.PortsOf(ctx, "victim"); len(ports) != 0 {
		t.Fatalf("multi-network rollback leaked ports: %v", ports)
	}
	// Addresses on both networks and the port must be reusable.
	eps2, err := m2.AttachAll(ctx, []string{"a", "b"}, AttachRequest{
		ContainerID: "new",
		Ports:       []PortMapping{{HostPort: 9100, ContainerPort: 80, Proto: "tcp"}},
	})
	if err != nil {
		t.Fatalf("resources of rolled-back txn must be reusable: %v", err)
	}
	if eps2[0].IP != eps[0].IP || eps2[1].IP != eps[1].IP {
		t.Fatalf("expected rolled-back addresses reused, got %v", eps2)
	}
}
