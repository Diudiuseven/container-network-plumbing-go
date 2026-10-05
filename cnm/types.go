// Package cnm 提供容器网络管理（Container Network Management）能力：
// 网络的创建/删除、容器接入/退出、地址分配、端口发布与网络内名称解析。
//
// 状态语义：
//   - 所有变更操作（接入/退出/删网）都是可序列化的：任意并发组合的最终
//     状态与某种串行执行顺序一致。
//   - 查询（Resolve/Inspect）只能观察到已提交的状态，永远看不到中间态。
//   - 进程在变更中途崩溃后，重启时通过日志（journal）恢复：
//     未完成的接入被回滚（释放已占资源），未完成的退出被完成（继续清理）。
package cnm

import (
	"fmt"
	"time"
)

// PortMapping 描述一条容器端口到宿主机端口的发布关系。
type PortMapping struct {
	Proto         string `json:"proto"` // "tcp" 或 "udp"
	HostPort      int    `json:"hostPort"`
	ContainerPort int    `json:"containerPort"`
}

// PortKey 唯一标识宿主机上一个被发布的端口。
type PortKey struct {
	Proto string `json:"proto"`
	Port  int    `json:"port"`
}

// Attachment 表示一个容器在某个网络上的一条接入关系。
type Attachment struct {
	ContainerID string        `json:"containerId"`
	Name        string        `json:"name"`
	Aliases     []string      `json:"aliases,omitempty"`
	IP          string        `json:"ip"`
	Ports       []PortMapping `json:"ports,omitempty"`
	JoinedAt    time.Time     `json:"joinedAt"`
}

// Network 表示一个受管网络及其地址池与接入关系。
type Network struct {
	Name    string `json:"name"`
	Subnet  string `json:"subnet"` // CIDR，例如 "10.10.0.0/24"
	Gateway string `json:"gateway,omitempty"`
	// Allocations 记录地址池分配：IP -> containerID。
	Allocations map[string]string `json:"allocations"`
	// Containers 记录接入关系：containerID -> Attachment。
	Containers map[string]*Attachment `json:"containers"`
}

// AttachRequest 描述把容器接入一个网络的请求。
type AttachRequest struct {
	Network string
	Aliases []string
	Ports   []PortMapping
}

// Txn 是写入状态文件的日志记录，用于崩溃恢复。
// 一次变更先落盘 Txn，再提交状态，最后清除 Txn。
type Txn struct {
	// Op 为 "attach" 或 "detach"。
	Op string `json:"op"`
	// ContainerID 是变更涉及的容器。
	ContainerID string `json:"containerId"`
	// Name 是容器名（attach 时用于解析注册）。
	Name string `json:"name,omitempty"`
	// Requests 是 attach 的原始请求（按网络）。
	Requests []txnAttach `json:"requests,omitempty"`
	// DetachNetworks 是 detach 涉及的网络列表。
	DetachNetworks []string `json:"detachNetworks,omitempty"`
	// Reserved 记录在 prepare 阶段已分配、提交前崩溃时需要回收的资源。
	Reserved []txnReservation `json:"reserved,omitempty"`
}

type txnAttach struct {
	Network string        `json:"network"`
	Aliases []string      `json:"aliases,omitempty"`
	Ports   []PortMapping `json:"ports,omitempty"`
}

type txnReservation struct {
	Network string `json:"network"`
	IP      string `json:"ip"`
}

// state 是持久化到磁盘的完整状态。
type state struct {
	Version  int                 `json:"version"`
	Networks map[string]*Network `json:"networks"`
	// Ports 记录宿主机端口占用：PortKey -> containerID。
	Ports map[PortKey]string `json:"ports"`
	// Pending 非空表示存在未完成的变更，重启后需要恢复。
	Pending *Txn `json:"pending,omitempty"`
}

func newState() *state {
	return &state{
		Version:  1,
		Networks: map[string]*Network{},
		Ports:    map[PortKey]string{},
	}
}

// MarshalText 实现 encoding.TextMarshaler，使 PortKey 可作为 JSON map 键。
func (k PortKey) MarshalText() ([]byte, error) {
	return []byte(k.Proto + ":" + itoa(k.Port)), nil
}

// UnmarshalText 实现 encoding.TextUnmarshaler。
func (k *PortKey) UnmarshalText(b []byte) error {
	s := string(b)
	i := -1
	for j := 0; j < len(s); j++ {
		if s[j] == ':' {
			i = j
			break
		}
	}
	if i <= 0 {
		return fmt.Errorf("invalid port key %q", s)
	}
	port, err := atoi(s[i+1:])
	if err != nil {
		return fmt.Errorf("invalid port key %q: %w", s, err)
	}
	k.Proto = s[:i]
	k.Port = port
	return nil
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

func atoi(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty number")
	}
	v := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("invalid number %q", s)
		}
		v = v*10 + int(c-'0')
	}
	return v, nil
}
