# container-network-plumbing-go

面向本地容器运行时的容器网络管理与接入库（`cnm` 包）。提供网络创建/删除、
容器多网络接入、地址池分配（IPAM）、宿主机端口发布、网络内名称解析，
并保证并发操作与进程异常中断下状态始终可解释、可恢复。

## 快速开始

```go
m, err := cnm.Open("/var/lib/myruntime/network") // 加载状态并自动崩溃恢复
if err != nil { /* ... */ }

_ = m.CreateNetwork("front", "10.1.0.0/24")
_ = m.CreateNetwork("back", "10.2.0.0/24")

// 原子地接入多个网络：要么全部成功，要么全部回滚。
err = m.Attach("container-id", "web", []cnm.AttachRequest{
    {Network: "front", Aliases: []string{"api"},
     Ports: []cnm.PortMapping{{Proto: "tcp", HostPort: 8080, ContainerPort: 80}}},
    {Network: "back"},
})

ips, _ := m.Resolve("front", "api") // 按容器名或别名解析
_ = m.Detach("container-id")        // 退出全部网络，回收地址与端口
```

## 网络状态语义

### 持久化与可见性

- 全部状态保存在 `Open(dir)` 指定目录下的 `state.json`，写入是原子的
  （临时文件 + fsync + rename）：任何时刻磁盘上要么是旧状态，要么是新状态。
- 每次变更先落盘再返回；查询（`Resolve`/`Inspect`/`Networks`）只读取
  已提交状态，**永远不会观察到未完成的中间态**（例如只接入了一半网络的容器）。

### 幂等性

- `CreateNetwork`：同名同子网重复创建返回成功；同名不同子网返回错误。
- `DeleteNetwork`：删除不存在的网络返回成功；删除时完整清理该网络上所有
  接入关系、地址分配与端口发布，不留残留。
- `Detach`：退出未接入的网络或不存在的容器返回成功。

### 多网络接入的原子性

`Attach` 接受多个网络的接入请求，分两个阶段执行：

1. **prepare**：校验全部请求（网络存在、未重复接入、**端口无冲突**），
   为每个网络分配地址并预留端口，写入日志（journal）后落盘；
2. **commit**：接入关系对查询可见，清除日志后落盘。

任一网络校验失败（包括端口冲突）时整体拒绝，容器不会停留在部分接入状态。
端口冲突一定在接入完成（commit）之前被发现。

### 地址分配（IPAM）

- 每个网络从自己的 CIDR 地址池独立分配，排除网络地址与广播地址。
- 分配是确定性的（最小可用地址），释放后的地址可被安全回收，
  回收不影响仍在使用的地址；池耗尽时接入被拒绝并整体回滚。

### 并发语义

所有变更在内部串行提交，查询使用读锁。任意并发的接入/退出/查询组合，
其最终状态与某种串行执行顺序一致（可序列化）。

### 崩溃恢复

进程在 prepare 与 commit 之间被强杀时，重启后 `Open` 自动按既定语义恢复：

| 未完成的操作 | 恢复动作 |
|---|---|
| `attach` | **回滚**：释放 prepare 预留的地址与端口，接入关系从未对外可见 |
| `detach` | **完成**：继续清理接入关系，回收地址与端口 |

恢复是幂等的：恢复过程中再次崩溃，下次 `Open` 会继续完成同样的恢复。
因此重启后不会重复占用地址，也不会残留无法回收的端口。

## 配置项

| 配置 | 说明 |
|---|---|
| `Open(dir)` 的 `dir` | 状态目录，`state.json` 存放位置，自动创建 |
| `CreateNetwork(name, subnet)` 的 `subnet` | IPv4 CIDR（如 `10.1.0.0/24`），至少 4 个地址；网关约定为子网第一个地址 |
| `AttachRequest.Network` | 目标网络名，必须已存在 |
| `AttachRequest.Aliases` | 网络内解析别名，可与容器名一起被 `Resolve` 解析 |
| `AttachRequest.Ports` | 端口发布列表：`Proto`（`tcp`/`udp`）、`HostPort`、`ContainerPort`（1–65535）；tcp/udp 端口空间相互独立 |

## 测试

```sh
go test ./...          # 全部测试
go test -race ./...    # 含竞态检测（并发场景）
```

测试覆盖：并发接入、地址冲突与回收、端口冲突、多网络接入原子性、
中途失败回滚、崩溃重启恢复。全部在普通用户权限下运行，不依赖外部网络
（状态目录使用 `t.TempDir()`，网络数据面为纯内存建模）。

## 代码结构

```
cnm/
  types.go   核心类型：Network / Attachment / PortMapping / Txn（恢复日志）
  store.go   状态原子持久化（临时文件 + fsync + rename）
  ipam.go    子网地址池：确定性分配与回收
  manager.go Manager：网络/接入/端口/解析/崩溃恢复
```
