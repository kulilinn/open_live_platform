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
- `POST /api/date/1vN/give/gifts` 赠送普通直播礼物，`GET /api/payment/points/get` 查询积分
- `GET|POST /api/live/room/rank/{room}` 查看礼物榜，`GET /api/date/anchor/income/log` 与 `/api/date/anchor/total/income` 查看主播礼物收益
- `GET /anchor/api/stat_fans_income`、`/anchor/api/stat_income_sometime` 与 `/anchor/api/get_income_details` 查看本账号的粉丝与结算收益
- `/api/nft/creation_card` 提供创世卡目录、权益、签名挑战及绑定，`/api/adorn` 提供装扮目录与装备管理
- `/api/dm` 查看私信会话、分页读取消息、标记已读、隐藏会话及发送私信
- `GET /api/admin/rooms`、`GET /api/admin/users`
- `POST /api/edge/events`

## 私信

`/api/dm` 使用登录令牌。已存在的粉丝与主播会话允许双方查看消息，标记已读只处理当前用户收到的消息，隐藏只影响当前用户的会话列表。会话收到新消息后恢复显示。会话、消息与用户 ID 在 JSON 响应中使用十进制字符串。

订阅服务在确认订阅与到期时调用内部 `SetDMSubscription`。首次授权创建会话及欢迎消息，订阅到期后可继续查看历史。旧 Rust 历史私信仍需单独导入。

设置 `DM_MODERATION_URL` 以接入私信内容审核。服务使用与 `/api/dm/send_message` 相同的 JSON 请求体向该地址发起 POST。审核端返回 HTTP 200 及 `{"pass":true}` 或 `{"pass":false}`。拒绝时返回业务码 `4061`。审核未配置或不可用时，发送请求返回 HTTP 503。

## 礼物与积分

配置 `GIFT_CATALOG_FILE`、`PAYMENT_HOST` 和 `PAYMENT_APPID` 后，普通直播礼物按服务端目录价格向原积分服务扣款。发送者必须已进入该直播场次，收件人必须是主播。扣款确认后保存订单和收益，向 H5 连接发送礼物消息。活动折扣、火箭、设备控制和私聊礼物依赖各自的配置或队列，当前普通礼物入口不处理这些购买。

需要可重试时，客户端给每次有意购买发送一个唯一的 `Idempotency-Key`，重试沿用同一键和请求。省略该头会按每次请求生成新订单。响应包含订单键及 `X-Payment-Order`，扣款结果未能确认时保留待核对订单，并返回 HTTP 409。对照积分服务确认该订单后再决定后续处理。生产扣款使用 PostgreSQL，内存模式仅用于单进程开发，其订单和重试记录在重启后失效。

## 钱包、订单与支付确认

`GET /api/payment/check/get` 读取当前用户的付款确认选项。`GET /api/payment/check/edit` 保存 `confirm_check` 和 `adult_check`，并接受可选的 `confirm_pay_check`、`confirm_country_check`。选项按用户保存三年。

钱包、订单查询、结账和退款由 `PAYMENT_HOST` 上的支付服务处理。将部署已验证的用户 API 方法及路径加入 `PAYMENT_PROXY_ROUTES`，用逗号分隔，例如 `GET /api/payment/实际钱包路径,POST /api/payment/实际订单路径`。Go 转发经过认证的 `USER_ID` 及配置的 `PAYMENT_APPID`、`PAYMENT_BORROW_APPID`。支付服务负责订单归属、价格、退款和回调验证，Go 转发其状态与响应体。平台内部扣款、批量积分查询、坏账和积分发放接口保留在服务边界内。

启用 `POST /api/payment/large_order/apply` 时，设置可信部署地区 `PAYMENT_LARGE_ORDER_REGION`，并按地区设置对应的 `PAYMENT_LARGE_ORDER_TEMPLATE_JA_JP`、`_ZH_CN`、`_ZH_TW` 或 `_EN_US`。通知服务沿用 `LOVEROSE_PUSH_HOST`、`LOVEROSE_PUSH_PATH`、`LOVEROSE_PUSH_EMAIL` 和 `LOVEROSE_PUSH_EMAIL_NAME`。服务确认零积分订单及通知请求后返回原有响应。使用 `Idempotency-Key` 重试时会读取已保存的结果，未确认的订单需与支付及通知服务对账。当前地区来自部署配置，IP 地区库需要单独接入。

`GET /pass/{pass_key}` 读取现有的 Apple Wallet 签名包。用 `uv run scripts/export_legacy_wallet_passes.py --app <原 APP_NAME>` 从各旧 Redis 分片导出 `pass_key|pkpass|APP_NAME` 条目，并合并为 `WALLET_PASS_INDEX_FILE` 指向的 JSONL 文件。导出记录保留原对象路径和绝对到期时间。配置 `AWS_PASSES_S3_BUCKET` 及现有 S3 访问配置后，Go 从原 bucket 读取 `/passes/{uid}/{pass_key}.pkpass`。应用启动时载入索引，已过期或缺失的链接返回业务码 `4005`。签发仍依赖原 Apple 证书与打包流程。

配置 `NFT_CATALOG_FILE` 指向包含 `cards`、`privileges`、`adorns` 数组的可信 JSON 目录。各项使用相应 API 的驼峰字段名，装扮目录另可配置 `effectTime` 和 `floatingScreenTime`。公共卡目录按状态筛选并隐藏大部分购买钱包地址。设置 `WEB3_HTTP` 和 `WEB3_CONTRACT_ADDRESS` 后，绑定核验以太坊签名及链上所有者。挑战绑定当前登录账号，五分钟有效，成功后只能使用一次。绑定状态和装扮保存到配置的 PostgreSQL，用户自查资料返回自己的绑定和装备。未上传头像的用户沿用 `USER_PHOTO_DEFAULT` 或 `USER_NFT_PHOTO_DEFAULT`。查询个人绑定或尝试装备时复核链上转移。

旧绑定可在导入账号后用 `LIVEMEDIA_MYSQL_DSN`、`DATABASE_URL`、`WEB3_HTTP`、`WEB3_CONTRACT_ADDRESS` 运行 `go run ./cmd/import-live-nft`。导入会核验原钱包仍持有对应卡片、保留有效装备，并尊重新库已经发生的解绑或转移。配置库存钱包地址 `WEB3_ACCOUNT` 和 `NFT_SYNC_TOKEN` 后，部署调度器可用该 Bearer 凭证调用 `/api/nft/creation_card/schedule_refresh`。只有在同一最终确认区块上同时确认卡片离开库存钱包并核实新所有者，服务才更新出售状态。可信目录另可包含 `inventory` 元数据数组，`/api/nft/creation_card/init_data_20250523` 从中加入链上已持有但尚未列出的卡片。两个入口均只读取区块链。

主播收益以已确认的普通直播礼物和导入的原 `settle_orders` 为来源。使用 `LIVEMEDIA_MYSQL_DSN`、`DATABASE_URL` 运行 `go run ./cmd/import-live-settlements` 可读取原 MySQL 的结算记录，多次导入同一记录不会重复计入。新增的订阅、付费帖子等交易要在有实际结算记录后才能计入。收益接口仅允许登录用户读取自己的账目。内部 `GET /api/earning/living_anchor_list` 要求 `EDGE_TOKEN` 的 Bearer 凭证，以及 `admin`、`mcn` 或 `innow` 的 `X-Inner-Service`。

## 管理后台

运营后台和 MCN 后台沿用各自的 Rust 路径。它们需要通过 `PLATFORM_ADMIN_ADDR` 和 `PLATFORM_MCN_ADMIN_ADDR` 显式设置各自的监听地址，并配置 `DATABASE_URL` 及 `ADMIN_ACCOUNTS_FILE`。例如各自监听回环地址并由部署网关接入。主业务监听不会注册兼容后台路径。原 Go `/api/admin` 查询接口只接受显式设置的 `ADMIN_TOKEN`。

`ADMIN_ACCOUNTS_FILE` 是由部署者从现有后台权限数据导出的 JSON 数组。条目包含 `id`、`area`、`passwordHash`、`admin`、`roles`、`permissions` 和 `routes`。`id` 是十进制字符串，`passwordHash` 是原 Rust Argon2id PHC 字符串。`area` 为 `admin` 时用 `username`、`nickName` 和 `tenant` 登录。运营接口要求 `operation-admin` 租户。`area` 为 `mcn` 时用 `email` 登录，`mcnId` 是十进制字符串，`manager` 代表只可查看本人的主播归属。菜单和权限来自导出文件，改动后重启服务。账号文件含凭据哈希，按凭据权限保管。旧后台 JWT 需重新登录，Go 后台会话的 access 和 refresh 分别有效两小时和七天。

运营后台支持用户和直播间查询、违规及操作记录分页、账号停用与启用、禁言、断播、禁播与解除。账号停用会撤回应用会话。礼物列表与分页读取当前礼物目录，`finance-admin` 租户可分页查询已确认的普通直播礼物流水。MCN 提供限定在当前机构的主播列表和分页，数据来自导入的 `backoffice_mcn_streamers`。历史后台权限和机构归属由部署者迁移。登录历史筛选、主播警告投递、其他结算类别和财务写操作仍依赖原后台服务。

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

同一用户再次兼容登录会使先前的 JWT 会话失效。`POST /api/auth/signout` 会撤销当前 JWT 会话签发的 refresh 和 access，并关闭该会话的全部 WebSocket。使用普通 Go 登录令牌退出时，只撤销该令牌并关闭它绑定的连接。PostgreSQL 模式会通知使用同一个数据库和 schema 的其他实例。

### 导入 Rust Redis 会话

切换前先导入保留原用户 ID 的账号数据，并为 Go 服务配置旧服务使用的 `JWT_SECRET`。暂停旧 Rust 服务的登录、令牌交换及登出后，从每个 Redis 源分片导出 `{uid}|auth_user|{APP_NAME}` 记录。以下示例将每个分片的 `REDIS_URL` 配置为其连接地址并生成一个 UTF-8 JSONL 文件。

```powershell
uv run scripts/export_legacy_redis_sessions.py --app $env:APP_NAME | Set-Content -Encoding utf8NoBOM redis-sessions.jsonl
Get-Content redis-sessions.jsonl | go run ./cmd/import-live-sessions -app $env:APP_NAME
```

导入读取目标库的 `DATABASE_URL`。每行包含 `key`、`value_base64` 和 RFC 3339 格式的 `expires_at`。导出保留 bincode 原值和 Redis 中的绝对过期时间，`APP_NAME` 未配置的旧服务使用 `unknown`。导入要求键、账号 ID 与邮箱对应，保留记录中 Rust JWT 原始邮箱大小写。原 JWT 的签名和 `exp` 继续验证，且会话最迟在 Redis 原过期时间失效。

导入结果分别统计成功、已过期、账号不匹配、已有会话及处于封禁期的账号。输入错误会给出行号并保留已成功写入的记录。重跑会跳过已有、已登出或已经被新 Go 登录替换的会话。每条旧 Redis 记录仅保存当时的 refresh ID，因此保留该 refresh 及其签发的 access。导出文件含账号标识和会话状态，请按凭据的访问权限保管。

### 邮箱验证与令牌交换

配置 `MAIL_SMTP_ADDR`（如 `smtp.example.org:587`）和 `MAIL_SMTP_FROM` 后，服务通过支持 STARTTLS 的 SMTP 发送验证码。`MAIL_SMTP_FROM` 可沿用旧环境的 `LR_FROM_MAIL`。SMTP 需要鉴权时同时设置 `MAIL_SMTP_USERNAME` 和 `MAIL_SMTP_PASSWORD`。多实例部署共用 `MAIL_CODE_SECRET`（如省略则使用 `JWT_SECRET`），`MAIL_CODE_EXPIRY` 的单位为秒，默认为 60 秒。

- `POST /api/auth/code` 接收 `email` 和 `signin`。注册时传 `false`，已有账号登录或重置密码时传 `true`。验证码由服务生成并发到该邮箱。有效期内重复请求返回剩余秒数，发送失败返回服务不可用。
- 邮件配置启用后，所有 `POST /api/auth/signup` 都需要注册用途的 `code`。`POST /api/auth/verifycode` 用 `email` 和 `code` 登录，注册用途的验证码也可以创建无公开密码的账号。`POST /api/auth/reset` 用已有账号用途的验证码和新 `password` 重置密码。验证码绑定邮箱，使用一次后失效，最多验证五次。
- 省略 SMTP 配置时，既有注册继续可用，邮件验证码、邮件登录和重置接口返回 HTTP 503。
- `POST /api/auth/exchange_code` 用有效的 access JWT 领取 5 分钟有效的一次性登录码。`POST /api/auth/exchange_token` 以 `{"code":"…"}` 换取一枚新的 refresh JWT。兑换会替换原 JWT 会话。原会话登出、过期或已被替换时，登录码也会失效。

## 订阅与私信

`/api/sub/anchor` 下的接口管理主播自己的免费、付费和一对一计划。`get_anchor_plan` 查看公开计划，`anchor_id: "0"` 查看自己的全部计划。`free_sub` 订阅主播的公开免费计划，`cancel_sub` 使用 `anchor_id`、`plan_id` 和 `reason` 退订。`get_sub`、`get_my_sub`、`sub_list` 及粉丝、订阅分页接口返回已保存的订阅关系。

免费订阅开通双方私信。付费订阅需要 PostgreSQL 和 `PAYMENT_HOST`、`PAYMENT_APPID`。Go 服务向原点数接口请求扣款，验证返回订单后保存付款凭据和订阅，并在同一数据库事务开通私信。仍有有效订阅时保留私信权限，最后一条订阅退订或到期后停止发送。上游结果不明确时保留待核对订单，同一计划的重试复用该订单号。

主播用 `anchor/promote` 和 `anchor/unpromote` 管理计划的限时促销。`pay_sub` 的 `promotion: 1` 保留旧服务的免费体验流程，限初次订阅者领取，体验期结束后停止对应权限。`anchor/create_buddle` 和 `anchor/del_buddle` 管理计划中的套餐展示。套餐付款需要独立的计价和结算能力。

设置 `SUBSCRIPTION_RENEW_TOKEN` 后，调度服务可持该 Bearer 令牌调用 `POST /api/sub/renew_sub`。每次检查最多 100 笔将在 UTC 后天零点前到期的有效订阅。续期要求先前已有验证通过的付费订单，且当前点数价格和最近一次实际扣款相同。点数不足时记录续费失败并取消续期。上游结果不明确时保留订单号供核对，并在结果确定前返回待核对状态。

## 从 LiveMedia 导入账号

`go run ./cmd/import-live-accounts` 从 `LIVEMEDIA_MYSQL_DSN` 读取 Rust 的 `user`、`email_login` 和 `user_violation_records`，写入 `DATABASE_URL` 指向的 PostgreSQL。源端使用只读账号，在停止旧服务写入后执行导入，再迁移 Redis 登录会话。

导入保留原有 64 位 ID、昵称、搜索名、头像、主播标记、语言、Argon2id 密码、封禁和禁言及其处罚记录。目标库中有相同 ID 或邮箱的本地账号时整批回滚。重复执行保留迁移后修改的密码和资料，并同步源处罚状态。输出中的 `restricted` 和 `without_email` 分别统计有处罚状态和没有邮箱的用户。没有邮箱的账号保留身份和匹配的旧 JWT 会话，可继续关联历史记录。

账号数据导入后，`go run ./cmd/import-live-interactions` 使用相同连接配置导入私信和通知。命令保留原有 ID、时间、每个会话成员的隐藏状态及已读状态。若旧会话允许发送且源 `subscription_user_sub` 中有对应的未取消、未过期权益，则保留发送权限直至该权益到期。输出统计成功导入、已存在和跳过的记录。跳过群聊、逻辑删除或缺少目标账号的记录。相同 ID 或成员对的本地数据冲突时回滚该次导入。

`go run ./cmd/import-live-subscriptions` 使用同一连接配置导入历史订阅计划、套餐展示、订阅期限、取消状态及历史结算记录。只有旧 `settle_orders` 与 `point_transactions` 能通过买家、主播、点数、交易引用和计划 ID 逐一对应时，才关联结算记录的 `verified_plan_id`。其余记录保留为可核对的历史数据。导入的历史订阅保留已有期限，后续由新 Go 订单验证付款后才具备自动续期资格。旧 Redis 促销的历史配置保留，重新开放限时体验需由该计划主播发布。

## 站内通知

登录用户使用 `GET /api/notice/page_notice?page=1&pageSize=20` 分页读取通知，`GET /api/notice/get_unread_notice_count` 查询未读数，`GET /api/notice/read_notice` 将自己的通知标记为已读。边缘服务可通过已鉴权的 `POST /api/edge/events` 投递 `type=notice` 事件，`notice` 对象中的 `id`、`userId` 和可选 `objectId` 使用十进制字符串。重复投递同一个通知 ID 会保留已读状态。

## Agora 音视频凭据与续签

在服务端同时设置 `AGORA_APP_ID` 和 `AGORA_APP_CERTIFICATE`，兼容进房请求 `POST /api/live/room/join/{room}?compat=rust` 会在 `data.rtcToken` 中返回签好的 Agora AccessToken2。两项均省略时仍可进房和文字聊天，`rtcToken.token` 为空。有效期由 `AGORA_TOKEN_EXPIRE` 指定，单位为秒，默认为 43200（12 小时）。

客户端用返回的 `channel`、`nid` 和 `token` 加入 Agora，`uid` 是平台的 64 位账号 ID。两种 ID 在响应中都是十进制字符串。主播的 RTC NID 为 `200000001`，观众同场重进沿用之前的 NID。同一房间重新开播后，观众按房间计数继续分配新号。

凭据即将到期时，用当前登录令牌请求 `GET /api/live/token/{channel}/{role}`。主播发布用 `1`，观众收看用 `2`。返回的 `data` 与进房的 `rtcToken` 同形，续签沿用同一 NID。离房后不能为该房间续签，已签发凭据按原有效期到期。Agora 项目如需在云端强制区分发布和订阅权限，请启用其主播鉴权功能。

兼容进房响应的 `data.languages` 返回本场已入房参与者的翻译语种，去重后排序，缺省语种为 `en`。主播通过开播或房间设置请求中的 `transLang` 配置房间语种，观众通过 `POST /api/user/set_info` 的 `transLang` 更新自己的翻译偏好。界面语言仍通过 `POST /api/user/set_language` 设置。

## 客户端进入聊天室

主播调用 `POST /api/live/room/start`。例如，请求体 `{"title":"晚场","password":"开场见"}` 会创建密码房，省略密码或传空字符串会创建公开房。开播响应、房间详情和列表的 `isLock` 表示本场是否需要密码。

直播中，主播可用自己的登录令牌调用 `POST /api/live/room/setting`。`{"subject":"下半场"}` 更新标题（也支持 `title`），`{"password":"新口令"}` 从下一次进房起使用新密码，`{"password":""}` 立即改为公开房。可以一次提交两个字段，省略或传 `null` 的字段保留原值。成功响应的 `status` 为 `"200"`，`data` 为更新后的房间。已进房的观众继续聊天。此接口仅用于已开播的房间。

观众用自己的登录令牌调用 `POST /api/live/room/join/{room}`。公开房可直接发空请求体。密码房传 `{"password":"开场见"}`，大小写和前后空格均参与匹配。房主可以免密进房。密码错误返回 403，验证成功会清零该观众的错误次数。连续输错五次后，该观众再进房返回 429。换成另一个密码时保留错误次数，主播取消密码或关播时清零全房计数。进房成功返回 204。

进房后，客户端用自己的 `Authorization: Bearer <token>` 请求 `GET /api/device/qrcode`，响应包含 `status` 和完整的 WebSocket 地址 `url`。默认 Go 模式示例中的 `token` 为当前用户的登录令牌，`roomId` 为开播接口返回的房间 ID。

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

默认 Go 模式的连接票据在当前服务进程内有效，限 60 秒内发起一次连接，重连时重新领取。已建立的连接可继续收发。通过反向代理提供服务时，把 `RELAY_PUBLIC_URL` 设为客户端可达的基址，例如 `wss://live.example.com`。直接连接服务时默认使用请求的主机及 TLS 状态生成地址。

兼容原浏览器 relay 时，在 WebSocket 请求中声明 `Sec-WebSocket-Protocol: h5relayer`。服务会先发送 `control` 用户帧，聊天使用原版的 `live_chat`、`queue`、`payload` 结构。主播主动关播时，H5 连接先收到 `channel: "room_status"`、`msgType: "O"`、状态值 `"0"` 的通知，再收到 WebSocket 正常关闭帧。使用 Go 票据并省略子协议的旧客户端继续使用 Go 的事件格式，关播事件为 `type: "close"`。

### Rust 连接票据和设备协议

同时设置 `RELAY_APP_KEY` 和 `RELAY_APP_SECRET`，`/api/device/qrcode` 会签发 Rust 格式的连接票据。票据可在同一登录会话内重复使用，默认有效期 24 小时，可用 `WS_TOEKN_EXPIRE` 设置秒数。活跃的 WebSocket 会延长票据有效期。`GET /relay/verify/{token}/{sig}` 可校验票据并返回应用标识与用户 ID。JWT 票据关联 refresh family，退出登录时，同一会话的 H5 和设备连接会收到 `1000 SERVICE_LOGOUT`。

使用 Rust 票据时，浏览器选择 `h5relayer`，设备选择 `devicerelayer`。设备可直接连接，直播浏览器连接则附带 `room_id` 或 `hash=/live/{房间号}`。同账号的设备状态和摇杆消息可以发送给浏览器，浏览器可以向同账号设备发送振动、波形模式和马达强度指令。已登录用户可调用 `GET /api/device/toyinfo` 刷新自己的设备状态。PostgreSQL 模式下，配置相同应用身份且连接同一 schema 的实例共享票据并转发设备消息。票据跨应用重启保留，达到 PostgreSQL 单条通知上限的大消息改用一分钟数据库引用投递。

配置 `RELAY_APP_ID` 启用 Rust 的 `POST /api/connector/acquire`、`/api/connector/vibrate` 和 `/api/connector/remote`。`RELAY_APP_USERS` 可填写该应用获准管理的现有用户 ID，多个 ID 用逗号分隔。这些绑定账号可以通过原应用签名或凭据申请连接和向自身设备发指令。其余账号在这两条路由上还需附带本人 Bearer 令牌。跨账号控制需由受控账号用本人 Bearer 限时授权，`expiry` 单位为秒，传 `0` 撤销。`duplex` 允许发出方同时控制自己和授权设备，反向控制需另行授权。

自动测试覆盖两实例领票、验证、重启、过期和注销，并用模拟 WebSocket 设备验证跨实例状态、24 KB 波形、账号隔离、应用绑定和远程授权的同意、过期与撤销。真实设备执行和前端联调仍待验证。

`POST /api/device/logs` 接受 `file`、`token`、`sig` 和 `data` 四个 multipart 字段。服务验证仍可使用的 Rust relay 票据，允许最多 5 MiB 写入 `toylogs/{appKey}/{用户 ID}/{file}`。日志复用上传接口的 S3 配置，测试覆盖跨实例票据、对象键、SigV4、过期和注销。

直播中最后一个主播 H5 连接断开时，观众收到同格式的状态值 `"3"`，已有连接、`liveId` 与聊天历史继续可用。主播重新进房或重连本场 H5 后，观众收到状态值 `"1"`。兼容房间详情和 `GET /api/live/room/online` 会返回相应的数字状态。

主播连续暂离 5 分钟后，服务自动关播并发送状态值 `"0"`。可用 `TEMP_OFFLINE_TIMEOUT` 设置正整数秒数，默认 `300`。每次重新进入暂离都重新计时。PostgreSQL 模式会持久化截止时间，服务重启后继续检查。主播连接登记每 5 秒续期，30 秒租约过期时，其他实例在确认同一场直播没有剩余主播连接后进入暂离。升级前的连接登记有 30 秒重连时间，升级时已经暂离的房间从迁移时刻起获得 5 分钟。

房主可通过 `GET /api/live/room/online?status=2` 进入 Rust 的门票房状态，通过 `status=1` 恢复普通直播。主播暂离后恢复原场时也会恢复状态 `2`，新开播从状态 `1` 开始。状态 `4` 来自后台封禁，房主接口不能自行设置或解除。普通请求不带 `status`，只查询当前状态。门票订单和核销依赖支付业务的独立授权流程。

房间状态接口沿用 Rust 的 `status/data` 包装。`kicked` 和 `banned` 的 `data` 是布尔值，`limited` 返回 `limit_ty`、`limit_reason`、`limit_allow`、`limit_duration`、`limit_total`。观众接口返回字符串形式的用户 ID，并提供昵称、管理员和禁言状态。

## 文本翻译和直播字幕

登录后向 `POST /api/translate` 发送 `{"text":"你好","to":["ja","en"]}`。服务按 Qwen、Azure、LibreTranslate、AWS、Qwen-MT 的顺序选择第一个启用的供应商。`llm:true` 直接选择 Qwen-MT。LibreTranslate 和 Qwen-MT 返回首个目标语种，其余路径返回所有目标。供应商异常时为对应目标返回原文，响应保留 `status/data.translations/data.detectedLanguage`。

| 供应商 | 环境配置 |
| --- | --- |
| Qwen | `USE_QWEN_TRANSLATE=1`，默认启用。读取 `OPENAI_API_BASE`、`OPENAI_API_KEY`、可选 `OPENAI_API_MODEL` |
| Azure | `USE_AZURE_TRANSLATE=1`，读取 `AZURE_TRANSLATOR_KEY`、可选 `AZURE_TRANSLATOR_ENDPOINT`、`AZURE_LOCATION` |
| LibreTranslate | `USE_LOCAL_TRANSLATE=1`，读取 `LIBRETRANSLATE_URL` |
| AWS | `USE_AWS_TRANSLATE=1`，读取 `AWS_TRANSLATE_ACCESS_KEY_ID`、`AWS_TRANSLATE_SECRET_ACCESS_KEY`、可选 `AWS_TRANSLATE_ENDPOINT_URL`、`AWS_TRANSLATE_REGION` |
| Qwen-MT | `USE_QWEN_MT_TRANSLATE=1`，共享 Qwen 配置，默认模型为 `qwen-mt-turbo` |

直播字幕需要现有 Agora RTC 的 `AGORA_APP_ID`、`AGORA_APP_CERTIFICATE`，以及 Agora REST 的 `AGORA_API_ID`、`AGORA_API_SECRET`。字幕 REST 默认使用 v2 agent 协议。已有旧项目可用 `AGORA_STT_API_VERSION=v1` 选择 builderToken 协议。直播开启 `trans` 时会尝试自动创建 v2 agent，语言优先取 `X-Lr-Stt-Lang`，其次取直播间 `transLang`。测试环境可通过 `AGORA_STT_BASE_URL` 替换 `https://api.agora.io`。

- `POST /api/live/room/stt-start/{room}` 接受语言数组。空数组使用 `X-Lr-Stt-Lang`。
- `GET /api/live/room/stt-query/{room}` 返回 Agora 任务状态，当前直播没有任务时返回 `STOPPED`。
- `POST /api/live/room/stt-update/{room}` 使用 `X-Lr-Stt-Lang` 更新语言。
- `POST /api/live/room/stt-stop/{room}` 停止任务。开始、更新和停止仅限房主。

PostgreSQL 保存字幕任务和所属直播或约会场次。关播后后台清理旧任务，两实例可以查询同一个任务。设置 `AGORA_STT_CALLBACK_SECRET` 后，可在 Agora Notifications 配置 `POST /api/live/stt/callback`，让已签名的 v7 结束通知清理对应的 agent。字幕文本通过 Agora RTC 数据流传输。自动测试使用本地 Agora 服务模拟及真实 PostgreSQL，覆盖新旧 REST 协议、任务状态、房主或约会双方权限、重启、回调和关播。真实 Agora 项目与字幕前端仍待验证。[Agora STT REST 快速入门](https://github.com/AgoraIO/docs-portal/blob/main/content/docs/en/realtime-media/speech-to-text/get-started/quickstart.md) · [Agora STT 数据流](https://github.com/AgoraIO/docs-portal/blob/main/content/docs/en/realtime-media/speech-to-text/build/process-transcription-data/parse-data.mdx)

## 1v1 约会和多人直播

主播通过 `POST /api/live/start` 开启接听，使用原 `POST /api/live/online` 刷新接听状态。粉丝通过 `POST /api/live/callin/{anchor}` 邀请主播，主播通过 `POST /api/live/callout/{audience}` 接听。Go 服务复用 Advanced 订阅授予和支付余额查询，按主播约会费率记录场次点数。与当前 Rust 路径一致，余额校验和场次点数记录不发起支付扣款。H5 私有推送只向对应用户发送邀请、接听、加入、离开和字幕语言。

1v1 主播和观众使用固定 RTC NID `100001` 和 `100002`。接听时双方各收到自己的 RTC 票据。`GET /api/live/token/{channel}/{role}` 仅为当前场次参与者续签。双方用 `/api/live/duration/{channel}` 刷新连线并查询时长，使用 `POST /api/live/leave/{channel}` 结束场次，可通过 `/api/live/checkout/{channel}` 查看结算摘要。每次新通话使用新频道，主播的多人直播继续走 `/api/live/room/*`。

1v1 字幕使用 `/api/live/stt-start/{channel}`、`stt-query`、`stt-update` 和 `stt-stop`，仅双方可访问。真实 PostgreSQL 测试覆盖大整数 ID、跨实例邀请和签发、订阅与余额校验、字幕、挂断及重启后场次记录，加密签发和外部服务使用本地模拟验证。

## 聊天记录

使用登录令牌向 `POST /api/live/room/messages` 发送 `{"id":"42","page":0,"size":20}`。`id` 是字符串房间号，页码从 0 开始，默认每页 100 条，最新的在前。`size: 0` 返回全部可见记录。响应沿用 Rust 的 `{"status":"200","data":[...]}`。每项的 `content` 为 `live_chat`，`queue` 为字符串房间号，`payload` 为数组。Go 每条文本单独成批，原前端可继续从 `payload[i].message.message` 读正文，从 `payload[i].message.userInfo.name` 读昵称。

房主和本场已进房成员可读。未进房、已离开、被踢或房间已关闭时，已登录用户得到 `data: []`。每场最多回看最近 100 条文本，连续 24 小时无人发言后重新计入历史，重新开播从空历史开始。PostgreSQL 模式支持服务重启后继续回看。

`POST /api/live/room/chat` 接受两种文本格式。

- Go 格式 `{"room_id":42,"text":"你好"}` 成功返回 HTTP 204。WebSocket 发送的聊天也可从历史接口读取。
- Rust 格式 `{"room":"42","channel":"chat","priority":0,"message":{"msgType":"C","msgGroup":"chat","userInfo":{"uid":"7","name":"小林"},"message":"你好"}}` 返回 `status`、布尔值 `data` 和数值 `msgId`。历史保留原消息体及其扩展字段，外层 `sender` 取自登录身份，`id`、`room`、`sender` 和 Unix 秒 `timestamp` 均为字符串。

## 直播间陪聊机器人

PostgreSQL 模式可通过 `USE_LIVEROOM_BOT` 与 `LIVEROOM_BOT_FILE` 开启陪聊。预先建好专用账号，并在配置文件中填写账号 ID 和语言。所有服务实例使用同一份配置。下面是一份最小示例。

```json
{
  "accounts": [{"id":"200001","locale":"ja"}],
  "scripts": {"ja":{"welcome":["ようこそ"],"gifts":{"1":["プレゼントありがとう"]}}}
}
```

机器人在公开房间开播后加入，每房间最多一个，账号一次只在一个房间中。持有已绑定创世神卡的观众进房时触发欢迎语。本场礼物成功结算且总点数达到 `BOT_GIFT_PRICE`（默认 100）时按礼物类型触发回复。机器人用自己的账号发送普通房间聊天，消息进入直播推送和历史。

文件可设置 `room_population`（加机器人前的观众上限，默认 20），以及 `entry_seconds`、`stay_seconds`、`welcome_delay_seconds`、`gift_delay_seconds`（分别默认 30、600、180、1 秒）。关播、踢出机器人或机器人离房会清理该账号的待发消息。Go 读取本地账号和脚本配置，自动点赞、日常闲聊和原 etcd/Redis 加权配置仍需迁移。

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

## 消息、上传和第三方登录

原 Feed 消息入口在 `/api/feed/message/`，包含 inbox、config、search、welcome、enabled、未读计数、已读和隐藏。订阅关系控制可私信状态，主播审核通过的欢迎语用于新建私信会话。已批准的私信通过接收方的 H5 连接定向推送。

配置 `KAFKA_BROKERS` 和 `REALTIME_NOTICE_TOPIC`、`REALTIME_NOTICE_GROUP` 或对应的 `DELAY_NOTICE_*` 后，服务启动 Kafka 消费者。`KAFKA_SSL` 启用 TLS。两个主题的默认过期时间分别为 3600 和 7200 秒，可用 `REALTIME_NOTICE_TTL` 和 `DELAY_NOTICE_TTL` 调整。

`GET /api/guid` 和 `POST /api/upload` 沿用编号从 0 开始的分片协议。分片可乱序到达。启用 PostgreSQL 后可跨实例接续，最后一个缺片到达后合并并签名写入对象存储。配置 `AWS_S3_BUCKET`、`AWS_S3_REGION`、`AWS_S3_ACCESS_KEY_ID`、`AWS_S3_SECRET_ACCESS_KEY` 和公开访问前缀 `BASE_STATIC_URL`。兼容 S3 的私有服务可设置 `AWS_S3_ENDPOINT_URL`。

`POST /api/auth/verifygoogle` 接受原有的 `{"token":"…"}`。配置 `GOOGLE_CLIENT_ID` 后，服务向 Google 核实 access token 所属应用、有效期和邮箱，使用稳定的 Google 用户标识关联本地账号。首次关联第三方托管邮箱时需要通过平台邮箱验证码，已有绑定可直接登录。启用 `JWT_SECRET` 后返回与邮箱登录相同的 refresh JWT。

分享发布、更新和删除沿用 `/api/shares/*` 与 5 MiB 图片上传，公开列表返回 CDN 图片地址和下一页偏移量。更新和删除要求登录账号是分享作者。用户反馈在 `/api/feedback/submit` 保存，可选附件存入 S3；`FEEDBACK_HOOK_URL` 配置通知接收方。每位用户每分钟可提交一次。

旧应用目录、版本、国家、帮助分类/问答、跑马灯及 PWA 主播清单通过 `go run ./cmd/import-live-catalog` 迁入 PostgreSQL。命令读取 `LIVEMEDIA_MYSQL_DSN` 和 `DATABASE_URL`，每次成功导入会在同一事务中替换整套公开资料。导入只选用公开列，应用密钥留在源库。

## 检查

```powershell
go test ./...
go vet ./...
```

配置 `OPEN_LIVE_TEST_DATABASE_URL` 后，`go test -count=1 ./internal/app` 会执行真实数据库回归测试。请使用专用测试数据库。

模块边界和源项目分析见 [MIGRATION_ANALYSIS.md](./MIGRATION_ANALYSIS.md)。
