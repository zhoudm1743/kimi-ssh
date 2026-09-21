# kimi-ssh

通过 MCP (Model Context Protocol) 管理 `~/.ssh/config` 里的 SSH 主机，为
[Kimi Code](https://www.kimi.com/code/) 打造，任何 MCP 客户端都可以用。

连接会保持住，所以 Agent 连一次就能在同一台服务器上连续执行命令，不用每条命令重连。

## 工具

| 工具 | 说明 |
| --- | --- |
| `ssh_list` | 列出从 `~/.ssh/config` 和 `~/.ssh/config.d/*.conf` 解析到的主机 |
| `ssh_connect` | 连接主机并保持会话 |
| `ssh_exec` | 在已连接的主机（或指定主机）上执行命令（默认 120 秒超时、输出有上限，见下文） |
| `ssh_status` | 查看当前活跃连接和当前主机 |
| `ssh_disconnect` | 断开连接 |

## 安装

### 直接下载二进制（无需 Go 环境）

到 [Releases](https://github.com/zhoudm1743/kimi-ssh/releases) 下载对应平台的文件：

```sh
# 以 linux/amd64 为例
curl -LO https://github.com/zhoudm1743/kimi-ssh/releases/latest/download/kimi-ssh_1.3.1_linux_amd64
chmod +x kimi-ssh_1.3.1_linux_amd64
mv kimi-ssh_1.3.1_linux_amd64 ~/.local/bin/kimi-ssh
```

提供 `linux/amd64`、`linux/arm64`、`darwin/amd64`、`darwin/arm64`、`windows/amd64`，
每个 release 都带 `checksums.txt`，可用 `sha256sum -c checksums.txt` 校验。

Linux 产物经 UPX 压缩（约 2 MB，未压缩约 5.5 MB）。代价是启动多几毫秒解压开销，
且 `go version -m`、`govulncheck` 之类工具无法再从二进制读出 Go 构建信息。
macOS 产物未压缩（UPX 不支持打包 Mach-O，强压会破坏 macOS 要求的代码签名），
Windows 产物也未压缩。

### 用 Go 安装

```sh
go install github.com/zhoudm1743/kimi-ssh@v1.3.1
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

仓库根目录的 `kimi.plugin.json` 把它声明成了 Kimi Code 插件（MCP 服务器名为
`ssh-manager`，`command` 从 `PATH` 里找 `kimi-ssh`），所以先把二进制装好
（上面的下载或 `go install` 二选一，确保所在目录在 `PATH` 里），再安装插件：

```sh
# 在 Kimi Code 里执行
/plugins install https://github.com/zhoudm1743/kimi-ssh
/reload
```

也可以从本地目录安装（清单和二进制都在仓库里时即可用）：

```sh
/plugins install /path/to/kimi-ssh
```

## SSH config 支持范围

已支持：`Host`（一行多别名、`*`/`?` 通配、`!` 否定）、`HostName`、`Port`、`User`、
`IdentityFile`、`ServerAliveInterval`、`ProxyCommand`、`ProxyJump`（仅解析存储）、
`Include`（支持通配符与嵌套）。

未支持：`Match` 块、`ProxyJump` 实际执行、`HostKeyAlgorithms`。

关键行为对齐 OpenSSH：

- 主机块没写 `HostName` 时，连到别名本身
- 主机块没写 `User` 时，用本机登录用户名
- 通配符 `Host` 参与解析：按文件顺序遍历所有匹配块，每个参数取**第一个**遇到的值
  （OpenSSH 的 "first obtained value wins"）。所以写在文件**开头**的 `Host *` 会盖住
  后面的具体块——这是 OpenSSH 的真实行为，本项目照实复现，没有"修正"它
- 含通配符的块（`Host *`、`Host *.example.com`、`Host !skip.example.com`）只贡献
  默认值，不会作为一台主机出现在 `ssh_list` 里；只有不含通配符的别名才是可连接主机
- 别名匹配大小写不敏感
- `Include` 的相对路径相对 `~/.ssh`（不是相对当前文件），`~` 展开为家目录，支持
  `conf.d/*.conf` 这类通配符；嵌套深度上限 8 层，同一文件只解析一次，自引用不会死循环
- `Host` 块里的 `Include` 会把被包含文件里没有 `Host` 行的指令继续加到当前块
- 主机密钥对照 `known_hosts` 校验

## 认证

认证方法按这个顺序提供，密钥优先、密码最后：

1. 显式 `IdentityFile`（配置了但读不出来仍然直接报错）
2. `ssh-agent`（读 `SSH_AUTH_SOCK`）
3. 默认密钥文件：`~/.ssh/id_ed25519`、`~/.ssh/id_ecdsa`、`~/.ssh/id_rsa`、
   `~/.ssh/id_dsa`，存在才尝试；解析失败（例如加密私钥）只跳过该文件并继续下一个，
   不会中断连接
4. 密码（只有配置了下面的密码来源时才注册）

密码来源按优先级：

- `SSH_ASKPASS`：值是**程序路径**，项目以 `<user>@<host>'s password: ` 作为唯一参数
  调用它，取 stdout 第一行作为密码。本来就没有 tty，所以不依赖 tty。
  `SSH_ASKPASS_REQUIRE=never` 时不调用它。
- `KIMI_SSH_PASSWORD`：字面密码，在 askpass 没配置、或 askpass 失败/没输出时使用。

两者都没有时不会添加密码认证方法——MCP 场景没有 tty 可以提示，不做交互式询问。
只要密码来源存在，就同时注册 `password` 与 `keyboard-interactive` 两种认证方法
（对每个 question 都回答同一个密码），因为不同服务器接受的不是同一种。密码在服务器
真的要密码时才会去取，公钥能过就不会启动 askpass 程序。

```sh
# 方式一：askpass 程序（$1 就是 "<user>@<host>'s password: "）
cat > ~/bin/ssh-pass.sh <<'EOF'
#!/bin/sh
secret-tool lookup service ssh host example.com
EOF
chmod +x ~/bin/ssh-pass.sh
SSH_ASKPASS=~/bin/ssh-pass.sh kimi-ssh

# 方式二：环境变量
KIMI_SSH_PASSWORD='...' kimi-ssh
```

密码只存在于进程内存中，不落盘、不打日志、不进入任何错误信息。但
`KIMI_SSH_PASSWORD` 本质是明文环境变量，同机其他进程、`/proc/<pid>/environ`、shell
history 都可能看到，请把它当作兜底手段而不是首选。

## 执行命令的限制

`ssh_exec` 在 `ssh_connect` 建立的会话上跑命令，并带几个护栏：

- 单条命令默认 120 秒超时，用 `KIMI_SSH_EXEC_TIMEOUT_MS`（毫秒）覆盖。超时由**远端**
  强制执行：命令会被包成 `timeout -k 5 <秒数> sh -c '<用户原始命令>'`，`timeout` 把命令
  放进自己的进程组，超时时端的是**整个进程组**（所以 `sleep`、子脚本这类孙进程也会被
  清掉，不会在服务器上留下还在跑的残留进程），`-k 5` 再补一刀 SIGKILL，兜住忽略
  SIGTERM 的命令。远端干净收尾后会话能正常收敛，**连接保持可用**，后续命令继续走同一个
  连接；结果的 `isError` 为 true，并说明是远端强制终止的。
- 客户端自己的 deadline 比远端晚 10 秒（远端 `-k` 那 5 秒宽限 + 5 秒余量），只作为
  兜底：正常情况下远端先动手，客户端不会触发。真正收不回来时（例如远端没有 `timeout`
  而命令又不肯退出），客户端会关掉会话，必要时把整条连接一起断开，结果里会说明需要
  重新连接。
- 远端 `timeout` 杀掉命令时退出码是 124，`-k` 触发 SIGKILL 时是 137。这两个码会被识别为
  超时而不是普通的非零退出。
- 极简系统可能没有 coreutils/busybox 的 `timeout`。这种时候（退出码 127 且 stderr 提示
  找不到 `timeout`）会**去掉包装重跑一次**，结果里明确警告「远端缺少 timeout，命令超时后
  可能无法被终止」；此后只剩客户端的 deadline 兜底，不再重试。
- stdout/stderr 各最多保留 1 MiB，用 `KIMI_SSH_EXEC_MAX_OUTPUT_BYTES` 覆盖。超出部分
  被丢弃，结果里会写「已截断，原始长度 N 字节」。管道仍然持续读空，所以远端命令不会
  因为写满缓冲区而卡住，也不会把整份输出读进内存。
- 远端非零退出码以 `EXIT: N` 出现在结果里，该次调用 `isError` 为 true；正常退出也会
  给出 `EXIT: 0`。超时（124/137）按上一条处理，不显示 `EXIT:`。
- 连接建立后每 30 秒发一次 `keepalive@openssh.com`（`ssh.Client.SendRequest`），
  配置里解析到 `ServerAliveInterval` 时用它（秒，0 表示关闭），断开连接时该 goroutine
  随之结束。

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
- 默认密钥文件（`~/.ssh/id_ed25519` → `id_ecdsa` → `id_rsa` → `id_dsa`）现在会被隐式
  加载并用于认证。不希望某个密钥被拿去认证时，把它移出 `~/.ssh` 或用显式
  `IdentityFile` 控制。
- 工具是只读的：无法通过 MCP 修改 `~/.ssh/config`。

## 许可证

Apache-2.0，见 `LICENSE`。
