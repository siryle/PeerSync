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
- 默认双向同步，用版本向量判断哪边更新。两边同时改同一个文件时，按内容校验值的固定顺序选定原名保留的版本，另一份保存在本机配置目录的 `indexes/<共享 ID>.conflicts/` 中，不写进共享目录，也不传播到其他设备。同一路径的同一份冲突版本会复用备份。
- 每个文件夹可设置「双向同步」「仅发送」「仅接收」。仅发送设备不接受远端修改或删除；仅接收设备绑定一个来源设备 ID，只接收该来源以「仅发送」模式提供的内容，不向任何设备发送本地文件。
- 文件权限和文件修改时间随版本同步；同内容的并发属性修改优先采用修改时间较大的属性，时间相同时采用权限数值较大的属性。目录同步权限，冲突时采用权限数值较大的值，不同步目录修改时间。权限受各系统文件系统支持范围限制。
- 启动时和运行中每五分钟完整校验文件内容，检测大小和修改时间均未变化的修改；常规扫描仍复用未变化文件的校验值。
- 删除会传播。一方改过、另一方删过时，保留改过的那份。
- 默认忽略 `.DS_Store`、`Thumbs.db`、`*.tmp`、编辑器临时文件，以及传输用的 `.peersync-tmp/`。
- 可以在共享目录里放 `.peersyncignore`，语法是 gitignore 的一个子集（`#` 注释、`*.log`、`secret/`、`/只在根目录.txt`）。这个文件本身会同步。

### 运行中的开发项目

在主要开发电脑上，将文件夹的「同步模式」设为「仅发送」。在运行项目的另一台设备上设为「仅接收」，从已发现的设备中选择来源，或填写开发电脑「这台设备」页面里的完整设备 ID。来源未连接，或未设为「仅发送」时，接收端不会覆盖文件；来源模式不匹配会显示在同步日志中。现有文件夹升级后仍保持双向同步，需要自行选择模式。

仅接收模式以来源的完整索引为准：本地新增但来源没有的文件会先备份再移除；本地修改会先备份再覆盖；来源删除了本地已修改的文件时，也会先备份再删除。本地删除的文件会从来源重新获取。未在本地修改的正常来源更新不生成冲突备份。忽略规则仍同时作用于接收、扫描和删除；依赖、缓存、构建产物、日志和运行数据库应按项目需要写入共享根目录的 `.peersyncignore`。

选中文件夹，点击「冲突备份」可查看这台设备保存的版本、预览文本、对照当前文件或下载原文件。文本预览最多 512 KiB、4000 行；二进制和更大的文件显示校验值及大小，并可下载。备份恢复前会校验内容，先保存当前版本，再恢复所选版本；如果当前文件在比较后又发生变化，会拒绝覆盖，需重新比较。恢复成功后会暂停这台设备的同步，检查结果后手动开始同步。仅接收模式重新开始后仍以来源为准，需要保留恢复结果时应先调整模式，或将备份下载到其他位置。

备份属于本机历史，不会自动清理。备份目录必须在共享目录之外；如果共享范围包含 PeerSync 的配置目录，需要将 `PEERSYNC_HOME` 移到共享范围之外。所有参与设备都应升级，旧客户端仍可能在共享目录中生成旧格式冲突副本；已有副本不会自动删除。

单向同步仍逐个文件应用更新，不保证整个项目在同一时刻切换版本；需要整项目一致部署时，应同步到暂存目录后再切换运行版本。

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

右上角菜单栏会显示从应用图标提炼的单色图标。点击红色关闭按钮或按 `⌘W` 只隐藏窗口，后台继续同步；点击 Dock 图标或菜单中的“显示主窗口”可恢复窗口。菜单栏提供暂停／恢复同步、重新连接、在 Finder 中打开同步文件夹、查看同步日志、设置和关于信息。暂停时禁用重新连接，文件夹和已连接设备数量会随实际状态更新，同一设备不会因参与多个文件夹而重复计数。

正常退出可以使用菜单栏图标菜单中的“退出 PeerSync”，也可以右键 Dock 图标选择“退出”；两者共用同一退出流程，先停止同步并关闭连接，再结束程序。关闭窗口仍只隐藏窗口、保持后台同步。应用菜单和 `⌘Q` 不提供退出入口。

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

新建或加入文件夹时，点击“选择”可以浏览容器内的目录，进入子目录或返回上一级，再点击“使用此文件夹”。默认从 `PEERSYNC_PATH`（镜像中为 `/sync`）开始；路径框已有内容时从该路径开始。宿主机目录需要先挂载到容器，例如 `-v /path/on/host:/sync`，页面中选择 `/sync`。

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
peersync share mode --mode send-only SHARE_ID
peersync share mode --mode receive-only --source SOURCE_DEVICE_ID SHARE_ID
```

`share create` 和 `share join` 同样支持 `--mode` 和 `--source`。命令行修改模式后需重启客户端；通过窗口修改会自动重新连接。模式值为 `send-receive`、`send-only`、`receive-only`。

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
| `PEERSYNC_MODE` | 新建或首次加入文件夹时的模式，默认 `send-receive` |
| `PEERSYNC_SOURCE_DEVICE_ID` | 新建或首次加入的仅接收文件夹所绑定的来源设备 ID |
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
