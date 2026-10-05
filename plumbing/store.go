package plumbing

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// journalOp kinds recorded in the write-ahead journal.
const (
	opAttachBegin  = "attach-begin"
	opAttachCommit = "attach-commit"
	opDetachBegin  = "detach-begin"
	opDetachCommit = "detach-commit"
)

// journalEntry is one intent record in the journal.
type journalEntry struct {
	Op          string        `json:"op"`
	TxnID       string        `json:"txnId"`
	Network     string        `json:"network"`
	ContainerID string        `json:"containerId"`
	Endpoint    *Endpoint     `json:"endpoint,omitempty"`
	Ports       []PortMapping `json:"ports,omitempty"`
}

// persistentState is the snapshot written to disk.
type persistentState struct {
	Networks map[string]*Network  `json:"networks"`
	Ports    map[string]portOwner `json:"ports"` // "proto/port" -> owner
}

// store persists state snapshots and the journal under dir.
type store struct {
	dir string
}

func newStore(dir string) *store { return &store{dir: dir} }

func (s *store) journalPath() string { return filepath.Join(s.dir, "journal.log") }
func (s *store) statePath() string   { return filepath.Join(s.dir, "state.json") }

// appendJournal durably records one journal entry (write + fsync).
func (s *store) appendJournal(e journalEntry) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(s.journalPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// readJournal returns all journal entries in order. A torn final line
// (crash mid-write) is ignored: the corresponding operation was never
// acknowledged and will simply be absent from the journal.
func (s *store) readJournal() ([]journalEntry, error) {
	f, err := os.Open(s.journalPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []journalEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e journalEntry
		if err := json.Unmarshal(line, &e); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			// Torn or corrupt trailing record: stop replay here.
			break
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// resetJournal truncates the journal. Called after a snapshot is saved.
func (s *store) resetJournal() error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.journalPath(), nil, 0o644)
}

// saveState atomically writes the state snapshot (tmp file + rename).
func (s *store) saveState(st persistentState) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.statePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := syncFile(tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.statePath()); err != nil {
		return fmt.Errorf("rename state: %w", err)
	}
	return syncDir(s.dir)
}

// loadState reads the snapshot; missing file yields an empty state.
func (s *store) loadState() (persistentState, error) {
	st := persistentState{
		Networks: map[string]*Network{},
		Ports:    map[string]portOwner{},
	}
	data, err := os.ReadFile(s.statePath())
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("corrupt state file: %w", err)
	}
	if st.Networks == nil {
		st.Networks = map[string]*Network{}
	}
	if st.Ports == nil {
		st.Ports = map[string]portOwner{}
	}
	return st, nil
}

func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
