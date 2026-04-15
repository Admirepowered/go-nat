# gonet

一个使用 Go 1.23 构建的 NAT 打洞工具，带服务端协调、客户端交互命令循环、本地端口自动映射，以及基于 UDP 打洞后的可靠隧道。

当前实现重点：

- 服务端命令：`gonet server`
- 客户端命令：`gonet client --connect 1.2.3.4:7000 --group demo`
- 客户端加入分组后自动获得唯一 ID
- `connect` 命令可按用户 ID 或 `#顺序号` 发起连接
- 默认把本地端口映射到对端机器上的 TCP 端口
- 可选把本地 UDP 端口映射到对端机器上的 UDP 端口
- 服务端同时监听 UDP 和 TCP 控制通道，客户端默认 UDP 控制，可选 `--control tcp`
- 打洞阶段会对服务端观测到的对端端口做一个小范围预测并同时发包
- 打洞失败时会自动回退到服务端中继，由控制通道转发隧道数据

## 构建

```bash
go build ./cmd/gonet
```

## 启动服务端

```bash
gonet server --listen :7000
```

## 启动客户端

```bash
gonet client --connect 1.2.3.4:7000 --group room-a
```

也可以使用 TCP 作为控制通道：

```bash
gonet client --connect 1.2.3.4:7000 --group room-a --control tcp
```

强制使用中继进行排障或受限网络测试：

```bash
gonet client --connect 1.2.3.4:7000 --group room-a --force-relay
```

## 客户端命令

进入客户端后可用：

```text
help
info
list
tunnels
connect <peer-id|#index> <target-port> [local-port] [tcp|udp]
exit
```

示例：

```text
connect c-a1b2c3 3389
connect #2 22 10022
connect #3 5353 15353 udp
```

含义：

- `connect c-a1b2c3 3389`
  - 自动分配本地端口，映射到对端 `127.0.0.1:3389/tcp`
- `connect #2 22 10022`
  - 在本地创建 `127.0.0.1:10022`，转发到分组内第 2 个用户的 `127.0.0.1:22/tcp`
- `connect #3 5353 15353 udp`
  - 在本地创建 `127.0.0.1:15353/udp`，转发到第 3 个用户的 `127.0.0.1:5353/udp`

## 说明

- 控制面支持 UDP 或 TCP。
- 对等隧道当前使用 UDP 打洞，然后在 UDP 上承载 `KCP + smux`。
- 当 UDP 打洞失败时，会自动改为“服务端控制通道中继虚拟数据包”，同样承载 `KCP + smux`。
- 默认目标应用协议为 TCP，可选 UDP。
- 远端目标默认认为在对端本机 `127.0.0.1:<target-port>`。
- “预测端口打洞”当前实现为：服务端根据观测到的实际公网端口，向两端下发包含当前端口和附近若干端口的候选列表，客户端会并发发起打洞。

## 局限

- 这是协调式 UDP 打洞实现，不保证所有 NAT 类型都能穿透，尤其是严格对称 NAT。
- 当前中继回退复用了控制通道，性能会低于直连。
- 目标服务地址目前固定为对端本机回环地址。
