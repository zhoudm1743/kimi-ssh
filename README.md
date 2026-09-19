# kimi-ssh

通过 MCP (Model Context Protocol) 管理 `~/.ssh/config` 里的 SSH 主机，为
[Kimi Code](https://www.kimi.com/code/) 打造，任何 MCP 客户端都可以用。

连接会保持住，所以 Agent 连一次就能在同一台服务器上连续执行命令，不用每条命令重连。

## 工具

| 工具 | 说明 |
| --- | --- |
| `ssh_list` | 列出从 `~/.ssh/config` 和 `~/.ssh/config.d/*.conf` 解析到的主机 |
| `ssh_connect` | 连接主机并保持会话 |
| `ssh_exec` | 在已连接的主机（或指定主机）上执行命令 |
| `ssh_status` | 查看当前活跃连接和当前主机 |
| `ssh_disconnect` | 断开连接 |

## 安装

### 直接下载二进制（无需 Go 环境）

到 [Releases](https://github.com/zhoudm1743/kimi-ssh/releases) 下载对应平台的文件：

```sh
# 以 linux/amd64 为例
curl -LO https://github.com/zhoudm1743/kimi-ssh/releases/latest/download/kimi-ssh_1.2.1_linux_amd64
chmod +x kimi-ssh_1.2.1_linux_amd64
mv kimi-ssh_1.2.1_linux_amd64 ~/.local/bin/kimi-ssh
```

提供 `linux/amd64`、`linux/arm64`、`darwin/amd64`、`darwin/arm64`、`windows/amd64`，
每个 release 都带 `checksums.txt`，可用 `sha256sum -c checksums.txt` 校验。

### 用 Go 安装

```sh
go install github.com/zhoudm1743/kimi-ssh@v1.2.1
```

### 从源码构建

```sh
make build      # 只构建本机平台
make snapshot   # 交叉编译全部平台到 dist/，并生成 checksums.txt
```

## 配置

Kimi Code 读取 `~/.kimi-code/mcp.json`（项目级为 `.kimi-code/mcp.json`）：

```json
{
  "mcpServers": {
    "ssh-manager": {
      "command": "/home/you/go/bin/kimi-ssh"
    }
  }
}
```

也可以打包成 Kimi Code 插件：在 `kimi.plugin.json` 旁边放二进制，
`mcpServers.<name>.command` 指向 `./bin/kimi-ssh` 即可。

## SSH config 支持范围

已支持：`Host`（一行多别名）、`HostName`、`Port`、`User`、`IdentityFile`、
`ProxyCommand`、`ProxyJump`（仅解析存储）。

未支持：`Include`、`Match` 块、`Host *` 默认项继承、`ProxyJump` 实际执行、
`HostKeyAlgorithms`。

关键行为对齐 OpenSSH：

- 主机块没写 `HostName` 时，连到别名本身
- 主机块没写 `User` 时，用本机登录用户名
- 密钥来自 `IdentityFile`，以及 `ssh-agent`（读 `SSH_AUTH_SOCK`）
- 主机密钥对照 `known_hosts` 校验

## 主机密钥校验

主机密钥会对照 `~/.ssh/known_hosts`、`~/.ssh/known_hosts2` 和
`/etc/ssh/ssh_known_hosts` 校验。未知主机会被拒绝；已记录但密钥不符的主机**始终**被拒绝。

设置 `KIMI_SSH_ACCEPT_NEW_HOST_KEY=1` 后，未记录的主机会被信任，并把密钥追加写入
`~/.ssh/known_hosts`（首次使用即信任，等价于 `StrictHostKeyChecking=accept-new`）。
**密钥不符的情况在该模式下依然会被拒绝。**

## 安全提醒

- 命令在远端 shell 中执行，权限就是配置用户的权限，没有命令白名单。请把 `ssh_exec`
  当作本地 shell 权限来对待。
- `ProxyCommand` 通过 `sh -c` 执行，因此能写 `~/.ssh/config` 的人/程序，在你建立连接时
  就能执行代码。
- 默认密钥文件（`~/.ssh/id_rsa`、`id_ed25519` 等）**不会**被隐式加载；请显式写
  `IdentityFile` 或使用 `ssh-agent`。
- 工具是只读的：无法通过 MCP 修改 `~/.ssh/config`。

## 许可证

Apache-2.0，见 `LICENSE`。
