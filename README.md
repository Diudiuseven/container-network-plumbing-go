# container-network-plumbing-go

面向本地容器运行时的容器网络管理与接入库。提供网络创建/删除、地址分配
（IPAM）、多网络接入、宿主机端口发布、网络内名称解析，并保证并发操作与
异常中断（进程被 kill）后状态始终可解释、可恢复。

纯 Go 实现，不依赖外部网络与 root 权限，全部状态落在本地状态目录中。

## 快速开始

```go
m, err := plumbing.Open("/var/lib/myapp/network") // 状态目录，可任意指定
if err != nil { ... }

ctx := context.Background()
_ = m.CreateNetwork(ctx, "web", "10.1.0.0/24")

ep, err := m.Attach(ctx, "web", plumbing.AttachRequest{
    ContainerID: "container-1",
    Name:        "web",                 // 可被 Resolve 解析的名字
    Aliases:     []string{"frontend"},  // 额外别名
    Ports: []plumbing.PortMapping{      // 发布到宿主机的端口
        {HostPort: 8080, ContainerPort: 80, Proto: "tcp"},
    },
})
// ep.IP 即为分配到的地址

ips, _ := m.Resolve(ctx, "web", "frontend") // 按名/别名发现在线容器地址
_ = m.Detach(ctx, "web", "container-1")     // 释放地址与端口
```

## API 一览

| 方法 | 语义 |
|---|---|
| `Open(dir)` | 打开/创建状态目录，并执行崩溃恢复 |
| `CreateNetwork(ctx, name, cidr)` | 创建网络；同名同子网重复创建为幂等空操作，同名不同子网报错 |
| `DeleteNetwork(ctx, name)` | 删除网络并清理其上全部接入关系、地址与端口；重复删除为幂等空操作 |
| `Attach(ctx, network, req)` | 接入单个网络，原子分配地址并发布端口 |
| `AttachAll(ctx, networks, req)` | 原子接入多个网络：全部成功或全部回滚 |
| `Detach(ctx, network, id)` | 断开接入并回收地址；容器离开最后一个网络时回收其端口；重复断开为幂等空操作 |
| `Resolve(ctx, network, nameOrAlias)` | 返回该网络内当前在线容器的地址（排序、去重前按名字/别名匹配） |
| `Network(ctx, name)` | 返回网络当前状态副本 |
| `PortsOf(ctx, containerID)` | 返回容器已发布的宿主机端口 |

## 网络状态语义

### 并发一致性
所有操作在内部互斥串行化：任意并发的接入、释放与查询组合，其最终
可观察状态都等价于某种串行执行的结果。查询（`Resolve`/`Network`/
`PortsOf`）不会观察到未完成的中间状态。测试使用 `-race` 验证。

### 地址分配（IPAM）
- 每个网络拥有独立地址池，从子网中按序分配，跳过网络地址与广播地址。
- 同一地址不会同时分配给两个容器；容器释放后地址可被安全回收，回收
  不影响仍在使用的地址。
- 地址池耗尽返回 `ErrNoAvailableIP`，且不会留下部分接入状态。

### 端口发布
- 端口冲突（`ErrPortInUse`）在任何状态变更**之前**被检测并拒绝，接入
  不会部分完成；一次发布中任一映射冲突则整批不生效。
- 端口按 `proto/hostPort` 唯一占用，tcp/udp 互不影响。
- 端口随容器生命周期回收：容器断开其最后一个网络（或所在网络被删除）
  时释放，释放后可被其他容器复用。

### 多网络接入的原子性
`AttachAll` 用一条日志事务覆盖所有目标网络：任一网络接入失败（地址
耗尽、端口冲突等）时，已完成的接入全部回滚，容器不会停留在部分接入
状态；进程中途崩溃时，恢复逻辑同样回滚整个事务。

### 崩溃恢复
状态目录下有两类文件：

- `state.json`：已提交状态的原子快照（临时文件 + rename 写入）。
- `journal.log`：预写式意图日志。每次接入/释放先记录 `*-begin`，状态
  落盘后记录 `*-commit`，随后截断日志。

`Open` 时重放日志：

| 中断点 | 恢复动作 |
|---|---|
| `attach-begin` 无 `attach-commit` | 回滚：移除端点、释放地址与端口 |
| `detach-begin` 无 `detach-commit` | 完成：移除端点、释放地址与端口 |
| begin + commit 齐全 | 忽略（操作已完成） |
| 末尾撕裂的半行记录 | 忽略（该操作从未被确认） |

恢复是幂等的：已提交的状态不会被重复回滚，已回收的资源不会被重复
释放；恢复后地址与端口立即可复用，不会重复占用或残留。

### 名称解析
`Resolve` 只返回**当前在线**端点的地址。容器扩缩容、断开或地址变化
后，下一次解析立即反映最新结果，不会返回已下线容器的地址。

## 配置项

| 配置 | 位置 | 说明 |
|---|---|---|
| 状态目录 | `Open(dir)` 参数 | 存放 `state.json` 与 `journal.log`，需可写 |
| 子网 | `CreateNetwork` 的 CIDR 参数 | 目前仅支持 IPv4；`/31`、`/32` 无可用主机地址 |
| 端口映射 | `AttachRequest.Ports` | `Proto` 仅支持 `tcp`/`udp`，端口范围 1–65535 |

## 测试

```sh
go test ./...          # 全部测试
go test -race ./...    # 含竞态检测
```

覆盖：并发接入/释放/查询收敛、地址冲突与回收、端口冲突、多网络原子
接入、中途失败回滚、进程重启与崩溃恢复（含日志撕裂）。全部测试在
普通用户权限下运行，不访问外部网络（状态目录使用 `t.TempDir()`）。
