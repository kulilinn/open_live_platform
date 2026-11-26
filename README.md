# open_live_platform

这是 `D:\my\LiveMedia` 的 Go 核心运行链路移植。项目用一个进程提供公开 API、直播房间、管理接口、边缘事件入口、WebSocket relay 和内部 gRPC。

需要 Go 1.24 或更高版本。

## 当前架构

- `cmd/platform` 只负责信号处理和启动。
- `internal/app` 负责配置、状态组装、路由注册和优雅停机。
- `internal/auth` 和 `internal/live` 定义账号与房间接口，并提供本地开发用的内存实现。
- `internal/store/postgres` 用 pgx 连接 PostgreSQL，保存账号、会话、房间状态和聊天记录。
- `internal/httpapi`、`internal/admin`、`internal/edge` 只负责 HTTP 适配。
- `internal/transport/httpx` 提供共享 JSON、错误、认证和路径工具。
- `internal/relay` 提供 WebSocket 适配。
- `api/platform/v1/platform.proto` 定义内部 gRPC 接口。

## 运行

```powershell
$env:PLATFORM_ADDR=':8080'
$env:PLATFORM_GRPC_ADDR='127.0.0.1:9090'
$env:ADMIN_TOKEN='admin'
$env:EDGE_TOKEN='edge'
$env:RELAY_SECRET='dev-secret'
go run .\cmd\platform
```

## 核心接口

- `GET /healthchecker` 和 `GET /status`
- `POST /api/auth/signup` 与 `POST /api/auth/signin`
- `GET /api/auth/session` 与 `POST /api/auth/signout`
- `POST /api/live/room/start` 开播，`POST /api/live/room/setting` 修改本场标题或密码
- `POST /api/live/room/list` 查询观众端房间列表
- `POST /api/live/room/join/{room}` 与 `POST /api/live/room/leave/{room}`
- `GET /api/live/token/{channel}/{role}` 在已加入的直播间续签 RTC 凭据
- `GET /api/live/room/info/{room}`、`/audiences/{room}`、`/admins/{room}`、`/bans/{room}`
- `GET /api/live/room/kicked/{room}/{uid}`、`/banned/{room}/{uid}`、`/limited/{room}`、`/audience/{room}/{uid}`
- `GET /api/live/room/online`、`/check_password/{room_id}`，以及 `POST /api/live/room/audiences/{room}` 分页查询
- `POST /api/live/room/chat` 发送文本，`POST /api/live/room/messages` 读取本场聊天历史
- `GET /api/device/qrcode` 领取 WebSocket 连接地址
- `GET /api/connector/{token}/{sig}` WebSocket 握手
- `GET /api/gift/list` 读取已配置的礼物目录
- `GET /api/admin/rooms`、`GET /api/admin/users`
- `POST /api/edge/events`

## 内部 gRPC

`platform.v1.PlatformService/GetStats` 替代原有的 `/internal/rpc/platform.stats`，接收空请求，返回账号总数 `users` 和完整房间列表 `rooms`。gRPC 独立监听，默认地址为 `127.0.0.1:9090`，可通过 `PLATFORM_GRPC_ADDR` 配置。前端继续使用 `PLATFORM_ADDR` 上的 HTTP 和 WebSocket 接口。

服务未启用反射。在仓库根目录使用 `grpcurl` 时，直接指定接口定义文件。

```powershell
grpcurl -plaintext -import-path . -proto api/platform/v1/platform.proto -d '{}' 127.0.0.1:9090 platform.v1.PlatformService/GetStats
```

Go 客户端和服务端绑定已随仓库提交。修改协议后，在仓库根目录用 `protoc`、`protoc-gen-go` 和 `protoc-gen-go-grpc` 重新生成。

```powershell
protoc --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative api/platform/v1/platform.proto
```

监听端口使用明文且没有内置鉴权。跨机器访问时，绑定到受限的内网地址，并通过防火墙或带 TLS 和鉴权的代理限制访问。

## 观众查找房间

使用登录令牌请求 `POST /api/live/room/list`。例如，`{"online":true,"page":1,"pageSize":20}` 取正在直播的前 20 个房间。改为 `"online":false` 可查已关播房间。分页请求省略 `online` 时，开播房间排在前面，同一状态按房间 ID 升序排列。

返回对象中的 `status` 为 `"200"`，`data` 为本页房间数组，`pages` 为符合筛选条件的总页数。页码从 1 开始，分页时默认取第 1 页、每页 20 个。超出最后一页时 `data` 为 `[]`。`pageSize` 为 0 时仅计数，`data` 为 `[]`，`pages` 为匹配房间数。

旧客户端发送空请求体或 `{}` 时，仍按 ID 升序收到全部房间的数组。

## JWT 会话

设置 `JWT_SECRET` 后，使用 `Accept: application/vnd.livemedia+json` 或 `?compat=rust` 登录会收到有效期 24 小时的 refresh JWT。用它请求 `GET /api/auth/session`，获取有效期 30 分钟的 access JWT。后续业务请求和领取 WebSocket 票据使用 access JWT。

同一用户再次兼容登录会使先前的 JWT 会话失效。`POST /api/auth/signout` 会撤销当前 JWT 会话签发的 refresh 和 access，已建立的 WebSocket 仍按房间连接规则管理。普通 Go 登录继续签发原有令牌。JWT 会话状态随 PostgreSQL 保留，现存 Rust/Redis 会话需单独迁移。

## Agora 音视频凭据

在服务端同时设置 `AGORA_APP_ID` 和 `AGORA_APP_CERTIFICATE`，兼容进房请求 `POST /api/live/room/join/{room}?compat=rust` 会在 `data.rtcToken` 中返回签好的 Agora AccessToken2。两项均省略时仍可进房和文字聊天，`rtcToken.token` 为空。有效期由 `AGORA_TOKEN_EXPIRE` 指定，单位为秒，默认为 43200（12 小时）。

客户端用返回的 `channel`、`nid` 和 `token` 加入 Agora，`uid` 是平台的 64 位账号 ID。两种 ID 在响应中都是十进制字符串。主播的 RTC NID 为 `200000001`，观众同场重进沿用之前的 NID。同一房间重新开播后，观众按房间计数继续分配新号。

凭据即将到期时，用当前登录令牌请求 `GET /api/live/token/{channel}/{role}`。主播发布用 `1`，观众收看用 `2`。返回的 `data` 与进房的 `rtcToken` 同形，续签沿用同一 NID。离房后不能为该房间续签，已签发凭据按原有效期到期。Agora 项目如需在云端强制区分发布和订阅权限，请启用其主播鉴权功能。

兼容进房响应的 `data.languages` 返回本场已入房参与者的翻译语种，去重后排序，缺省语种为 `en`。主播通过开播或房间设置请求中的 `transLang` 配置房间语种，观众通过 `POST /api/user/set_info` 的 `transLang` 更新自己的翻译偏好。界面语言仍通过 `POST /api/user/set_language` 设置。

## 客户端进入聊天室

主播调用 `POST /api/live/room/start`。例如，请求体 `{"title":"晚场","password":"开场见"}` 会创建密码房，省略密码或传空字符串会创建公开房。开播响应、房间详情和列表的 `isLock` 表示本场是否需要密码。

直播中，主播可用自己的登录令牌调用 `POST /api/live/room/setting`。`{"subject":"下半场"}` 更新标题（也支持 `title`），`{"password":"新口令"}` 从下一次进房起使用新密码，`{"password":""}` 立即改为公开房。可以一次提交两个字段，省略或传 `null` 的字段保留原值。成功响应的 `status` 为 `"200"`，`data` 为更新后的房间。已进房的观众继续聊天。此接口仅用于已开播的房间。

观众用自己的登录令牌调用 `POST /api/live/room/join/{room}`。公开房可直接发空请求体。密码房传 `{"password":"开场见"}`，大小写和前后空格均参与匹配。房主可以免密进房。密码错误返回 403，验证成功会清零该观众的错误次数。连续输错五次后，该观众再进房返回 429。换成另一个密码时保留错误次数，主播取消密码或关播时清零全房计数。进房成功返回 204。

进房后，客户端用自己的 `Authorization: Bearer <token>` 请求 `GET /api/device/qrcode`，响应包含 `status` 和完整的 WebSocket 地址 `url`。浏览器示例中的 `token` 为当前用户的登录令牌，`roomId` 为开播接口返回的房间 ID。

```javascript
const response = await fetch("/api/device/qrcode", {
  headers: { Authorization: `Bearer ${token}` }
});
const { url } = await response.json();
const address = new URL(url);
address.searchParams.set("room_id", roomId);
const socket = new WebSocket(address);
socket.onmessage = event => console.log(JSON.parse(event.data));
socket.onopen = () => socket.send(JSON.stringify({ type: "chat", text: "你好" }));
```

连接票据在当前服务进程内有效，限 60 秒内发起一次连接，重连时重新领取。已建立的连接可继续收发。通过反向代理提供服务时，把 `RELAY_PUBLIC_URL` 设为客户端可达的基址，例如 `wss://live.example.com`。直接连接服务时默认使用请求的主机及 TLS 状态生成地址。

兼容原浏览器 relay 时，在 WebSocket 请求中声明 `Sec-WebSocket-Protocol: h5relayer`。服务会先发送 `control` 用户帧，聊天使用原版的 `live_chat`、`queue`、`payload` 结构。旧客户端省略子协议时继续使用 Go 的 `type`、`text` 消息格式。当前兼容层先覆盖用户首帧和文本聊天。

房间状态接口沿用 Rust 的 `status/data` 包装。`kicked` 和 `banned` 的 `data` 是布尔值，`limited` 返回 `limit_ty`、`limit_reason`、`limit_allow`、`limit_duration`、`limit_total`。观众接口返回字符串形式的用户 ID，并提供昵称、管理员和禁言状态。

## 聊天记录

使用登录令牌向 `POST /api/live/room/messages` 发送 `{"id":"42","page":0,"size":20}`。`id` 是字符串房间号，页码从 0 开始，默认每页 100 条，最新的在前。`size: 0` 返回全部可见记录。响应沿用 Rust 的 `{"status":"200","data":[...]}`。每项的 `content` 为 `live_chat`，`queue` 为字符串房间号，`payload` 为数组。Go 每条文本单独成批，原前端可继续从 `payload[i].message.message` 读正文，从 `payload[i].message.userInfo.name` 读昵称。

房主和本场已进房成员可读。未进房、已离开、被踢或房间已关闭时，已登录用户得到 `data: []`。每场最多回看最近 100 条文本，连续 24 小时无人发言后重新计入历史，重新开播从空历史开始。PostgreSQL 模式支持服务重启后继续回看。

`POST /api/live/room/chat` 接受两种文本格式。

- Go 格式 `{"room_id":42,"text":"你好"}` 成功返回 HTTP 204。WebSocket 发送的聊天也可从历史接口读取。
- Rust 格式 `{"room":"42","channel":"chat","priority":0,"message":{"msgType":"C","msgGroup":"chat","userInfo":{"uid":"7","name":"小林"},"message":"你好"}}` 返回 `status`、布尔值 `data` 和数值 `msgId`。历史保留原消息体及其扩展字段，外层 `sender` 取自登录身份，`id`、`room`、`sender` 和 Unix 秒 `timestamp` 均为字符串。

## 礼物目录

将 `GIFT_CATALOG_FILE` 设为礼物 JSON 文件的路径，例如内容为 `[{"id":"101","name":"花束","price":20,"giftSort":1}]`。服务在启动时加载目录。登录后请求 `GET /api/gift/list` 会得到 `status/data`，礼物按 `giftSort` 升序排列。没有配置目录或请求带有 `?all=false` 时，`data` 为空数组。

## PostgreSQL

准备一个专用数据库，例如 `open_live_platform`。连接用户需要在目标 schema 上有建表权限。以下命令用于本地 PowerShell，连接字符串中的账号和密码换成自己的值。

```powershell
$env:DATABASE_URL='postgres://app:password@127.0.0.1:5432/open_live_platform?sslmode=disable'
go run ./cmd/platform
```

服务启动时从内置的[数据库迁移目录](internal/store/postgres/migrations)按编号加载 SQL，并在目标 schema 的 `schema_migrations` 中登记已应用版本。旧 Go 库首次升级会执行已有幂等脚本和回填，后续启动只执行新增版本。迁移和版本登记使用同一个事务，失败会一并回滚。新增迁移只需添加新的编号 SQL 文件。设置了 `DATABASE_URL` 时，连接或迁移失败会终止启动。省略该变量时使用内存模式，重启即清空。已有 Rust/MySQL 数据的导入需要单独迁移。

PG 模式下，账号、登录会话、房间、成员、管理员、禁言、踢出和聊天记录可跨重启保留。房间密码以加盐哈希保存，本场密码及观众输错次数随服务重启保留，关播时清除。旧版本的房间升级后按公开房处理。重新开播时按房间业务规则重置本场成员和管理状态。

每次重新开播都会分配独立的 64 位 `liveId`。房间详情返回本场 ID，兼容进房响应还返回开播时间。关播后的时长保持固定。旧库中已经在线的房间用最后更新时间估算首次升级的开播时间。

连接同一 PostgreSQL 数据库和 schema 的 Go 实例会互相广播房间事件。服务通过独立的 `LISTEN` 连接接收通知，数据库连接地址须支持长连接会话。监听中断时旧订阅会结束，客户端重连后可回查持久化聊天历史。

升级到聊天历史版本时，数据库保留已有消息。旧版本未记录聊天的场次边界，升级时在线房间仅展示最后一次房间更新后可确认属于本场的消息。之后的开播、关播会记录精确边界。

## 检查

```powershell
go test ./...
go vet ./...
```

配置 `OPEN_LIVE_TEST_DATABASE_URL` 后，`go test -count=1 ./internal/app` 会执行真实数据库回归测试。请使用专用测试数据库。

模块边界和源项目分析见 [MIGRATION_ANALYSIS.md](./MIGRATION_ANALYSIS.md)。
