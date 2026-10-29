# PeerSync

PeerSync 在几台电脑之间同步同一个文件夹。服务端只负责让客户端互相发现，目录内容在客户端之间直接传输，不经过服务端。

![PeerSync](internal/ui/assets/logo.png)

客户端是同一个 Go 程序。macOS 和 Windows 打开窗口，Docker 打开网页。三边都会显示同步日志：连上谁、收到或发出哪个文件、删除、冲突和失败。

## 它怎么工作

```text
客户端 A                         发现服务                         客户端 B
   |                          （Docker 里运行）                        |
   |---- 上线、文件夹凭证 -------------------------------------------->|
   |<--- 房间里还有谁、对方的公网 IP ----------------------------------|
   |                                                                 |
   |---- 加密后的地址（服务端读不懂） --------------------------------->|
   |                                                                 |
   |<================ 直接连接：TCP，或 UDP 打洞后的 QUIC ============>|
   |                    文件块只走这条连接                              |
```

发现服务提供两样东西：

- `8080/tcp`：WebSocket。交换“谁在线”，并转发很小的加密地址消息。
- `3478/udp`：地址反射。客户端发一个包，服务端把看到的公网地址回给它。这个包不会转交给别人。

连接优先级：

1. 同一局域网或同一 Docker 网络里，直接连对方公布的地址。
2. 两边都在 NAT 后面时，用 UDP 打洞，再在打通的路径上跑 QUIC。
3. 打洞失败就保持离线并重试。服务端不会代为转发文件。对称 NAT 往往打洞失败，这时需要在其中一台机器上做端口映射。

每个设备有自己的 Ed25519 身份。每个共享文件夹有一把独立的文件夹密钥，放在邀请码里。直连之后，双方要证明自己持有设备私钥和文件夹密钥，并且这个证明绑定在当前这条 TLS 会话上。邀请码等于成员资格，拿到邀请码的人可以加入这个文件夹。

同步规则：

- 递归同步目录里的文件，空目录也会同步。
- 用版本向量判断哪边更新。两边同时改同一个文件时，保留两份：原名是对方的版本，本机版本改名为 `文件名.sync-conflict-<设备>-<时间>.扩展名`。
- 删除会传播。一方改过、另一方删过时，保留改过的那份。
- 默认忽略 `.DS_Store`、`Thumbs.db`、`*.tmp`、编辑器临时文件，以及传输用的 `.peersync-tmp/`。
- 可以在共享目录里放 `.peersyncignore`，语法是 gitignore 的一个子集（`#` 注释、`*.log`、`secret/`、`/只在根目录.txt`）。这个文件本身会同步。

## 服务端

服务端和客户端分别构建为 `peersync-server:latest` 和 `peersync-client:latest`：

```bash
docker build -f deploy/Dockerfile.server -t peersync-server:latest .
docker build -f deploy/Dockerfile.client -t peersync-client:latest .
```

在仓库根目录：

```bash
docker compose up -d --build
```

服务起来之后：

- 发现地址：`ws://<这台机器的 IP>:8080`
- 健康检查：`http://<IP>:8080/health`

防火墙放行 `8080/tcp` 和 `3478/udp`。不要把文件同步端口理解成服务端端口；文件不进这台机器。

不用 Docker 时，在 Linux 上运行编译好的 `peersync-server`：

```bash
peersync-server --listen :8080 --stun :3478
```

## 客户端

配置和设备密钥在系统配置目录里，可以用 `PEERSYNC_HOME` 改掉：

| 系统 | 默认目录 |
| --- | --- |
| macOS | `~/Library/Application Support/peersync` |
| Windows | `%AppData%\peersync` |
| Linux | `$XDG_CONFIG_HOME/peersync` 或 `~/.config/peersync` |

下面把发现服务写成 `ws://192.168.1.10:8080`，换成你的地址。

### macOS

双击 `PeerSync.app`。窗口里填写这台设备的名称和发现服务地址，然后新建文件夹，或用邀请码加入。新建后出现的邀请码是秘密，交给其他设备即可。选中一个文件夹后，下面的同步日志会记下这台电脑收到、发出、删除和冲突的文件。

在仓库里编译应用（需要本机的 Xcode 命令行工具）：

```bash
make app
open bin/PeerSync.app
```

不带参数启动会打开窗口。命令行仍然用来在后台跑，适合 launchd：

```bash
make client
./bin/peersync init --name my-mac
./bin/peersync share create --path "$HOME/Sync" --label docs --server ws://192.168.1.10:8080
./bin/peersync serve
```

`share create` 会打印一行 `invite: peersync1_...`。

另一台电脑：

```bash
peersync share join --invite 'peersync1_...' --path "$HOME/Sync"
peersync serve
```

登录后后台运行可以用 launchd。把下面存成 `~/Library/LaunchAgents/peersync.plist`，路径改成你的二进制和家目录：

```xml
<plist version="1.0">
<dict>
  <key>Label</key><string>peersync</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/peersync</string>
    <string>serve</string>
  </array>
</dict>
</plist>
```

然后 `launchctl load ~/Library/LaunchAgents/peersync.plist`。

### Windows

双击 `PeerSync-windows-amd64.exe`（ARM 用 `PeerSync-windows-arm64.exe`）。这是带窗口的程序，不带黑色命令行。界面和 Mac 相同：设备名称、发现服务、新建或加入文件夹，以及每个文件夹的同步日志。

窗口使用系统里的 Microsoft Edge WebView2。Windows 11 和较新的 Windows 10 已经自带。如果打开时提示缺少运行时，安装微软的 WebView2 Evergreen Runtime。

在别的系统上交叉编译：

```bash
make windows-gui
```

第一次同步如果防火墙询问，允许专用网络和公用网络。打洞需要收到对方的 UDP 回包。

命令行版是 `peersync-windows-amd64.exe`，给脚本和任务计划用：

```powershell
.\peersync-windows-amd64.exe init --name my-pc
.\peersync-windows-amd64.exe share join --invite "peersync1_..." --path "$env:USERPROFILE\Sync" --server ws://192.168.1.10:8080
.\peersync-windows-amd64.exe serve
```

开机后自动打开窗口：把 `PeerSync-windows-amd64.exe` 放进启动文件夹，或在任务计划程序里于登录时启动它，参数留空。

### Docker 客户端

同一台 Docker 主机上的两个目录可以对齐。网页开在所有网卡上，所以要先设界面口令。口令至少 8 个字符，程序不会生成，日志里也不打印。

```bash
PEERSYNC_UI_TOKEN='你自己的口令' docker compose --profile demo up -d --build discovery client-a
docker compose logs client-a
```

日志里有 `INVITE=peersync1_...`，还有 `UI=http://0.0.0.0:8384/`。在跑 Docker 的这台机器上打开 `http://127.0.0.1:8384/`，输入同一个口令。页面和 Mac、Windows 是同一个，同步日志也在里面。`client-b` 的网页在 `http://127.0.0.1:8385/`。

第二个客户端：

```bash
PEERSYNC_UI_TOKEN='你自己的口令' PEERSYNC_INVITE='peersync1_...' docker compose --profile demo up -d client-b
```

`./data/a` 和 `./data/b` 会对齐。

客户端容器要连到别的机器上的 PeerSync 时，发布端口，让打洞用的是稳定端口：

```bash
docker run -d --name peersync \
  -e PEERSYNC_SERVER=ws://192.168.1.10:8080 \
  -e PEERSYNC_NAME=nas \
  -e PEERSYNC_INVITE='peersync1_...' \
  -e PEERSYNC_PATH=/sync \
  -e PEERSYNC_HOME=/config \
  -e PEERSYNC_UI=0.0.0.0:8384 \
  -e PEERSYNC_UI_TOKEN='你自己的口令' \
  -p 42420:42420/tcp \
  -p 42420:42420/udp \
  -p 8384:8384 \
  -v peersync-config:/config \
  -v /path/on/host:/sync \
  peersync-client:latest
```

镜像默认把网页开在 `0.0.0.0:8384`。不映射 `8384` 就只能在容器里面访问。想改地址时设置 `PEERSYNC_UI`，例如 `127.0.0.1:8384` 只给本机。

镜像用仓库根目录构建：

```bash
docker build -f deploy/Dockerfile.client -t peersync-client:latest .
```

Linux 上如果只和本机其他容器同步，用 Docker 网络即可，不必映射端口。跨公网时映射 UDP。

## 常用命令

```bash
peersync id                 # 本机设备 ID
peersync status             # 正在运行的 serve 会每 2 秒刷新状态
peersync share ls
peersync share rm SHARE_ID  # 只从本机配置移除，不删除目录
```

`serve` 的环境变量：

| 变量 | 作用 |
| --- | --- |
| `PEERSYNC_HOME` | 配置目录 |
| `PEERSYNC_SERVER` | 发现服务，例如 `ws://host:8080` |
| `PEERSYNC_NAME` | 设备名 |
| `PEERSYNC_LISTEN` | 本机 TCP/UDP 端口，默认 42420 |
| `PEERSYNC_PATH` | Docker 里要同步的目录，默认 `/sync` |
| `PEERSYNC_CREATE=1` | 还没有文件夹时创建一个，并把邀请码打到标准输出 |
| `PEERSYNC_INVITE` | 用邀请码加入 |
| `PEERSYNC_LABEL` | 文件夹显示名 |
| `PEERSYNC_VERBOSE=1` | 调试日志 |
| `PEERSYNC_UI` | 网页地址。`serve` 默认 `127.0.0.1:8384`；Docker 镜像默认 `0.0.0.0:8384` |
| `PEERSYNC_UI_TOKEN` | 网页口令。监听地址不是本机回环时必须设置，至少 8 个字符。改口令后重新启动即可 |

## 限制

- 没有中继。两边都是对称 NAT，又没有端口映射时，连不上。
- 不跟随符号链接，不同步 Windows 保留设备名。
- 单个文件按 128 KiB 分块。索引放在配置目录，不放进同步目录。
- 发现服务重启后客户端会自动重连。已经连上的直连不会因为发现服务短暂断开而拆掉。
- 删除记录会留在索引里，避免一台长期离线的设备把已删除文件又推回来。

## 开发

需要 Go 1.26。

```bash
go test ./...
make app          # macOS 窗口，PeerSync.app
make windows-gui  # Windows 窗口，可在 macOS 上交叉编译
make build-all    # 再加各平台命令行和无界面服务端
```

`go test ./internal/node` 会在本机拉起发现服务和两个客户端，同步、修改、删除一个 300 KiB 的文件，并检查发现服务转发的字节远小于文件本身。
