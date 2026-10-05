package cnm

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// store 负责状态的持久化：原子写入与加载。
type store struct {
	path string
}

func newStore(path string) *store { return &store{path: path} }

// load 读取状态文件；文件不存在时返回空状态。
func (s *store) load() (*state, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return newState(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	st := newState()
	if err := json.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("decode state: %w", err)
	}
	// 防御：反序列化后保证 map 非空，避免后续 nil 写入 panic。
	if st.Networks == nil {
		st.Networks = map[string]*Network{}
	}
	if st.Ports == nil {
		st.Ports = map[PortKey]string{}
	}
	for _, n := range st.Networks {
		if n.Allocations == nil {
			n.Allocations = map[string]string{}
		}
		if n.Containers == nil {
			n.Containers = map[string]*Attachment{}
		}
	}
	return st, nil
}

// save 原子地写入状态：先写临时文件并 fsync，再 rename，最后 fsync 目录。
// 崩溃时要么看到旧状态，要么看到新状态，不会看到半写的文件。
func (s *store) save(st *state) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp state: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 后该路径已不存在，失败可忽略
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("fsync temp state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp state: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("rename state: %w", err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync() // 尽力而为；部分文件系统不支持目录 fsync
		_ = d.Close()
	}
	return nil
}
