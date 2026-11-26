# LiveMedia Go 移植任务清单

## 当前基线

- 仓库目录为 `D:\my\open_live_platform`。
- 当前主线已接入 Go 直播核心、PostgreSQL 持久化、H5 文字聊天、场次数据和跨实例房间广播。
- 已验证 `go test ./...`、真实 PostgreSQL 回归、`go vet ./...` 和 Go 编译。
- 保留 64 位用户、房间、消息 ID。未修改任何 `CMakeLists.txt` 或 `prcmake` 文件。

## 本轮优先完成

- [x] Rust 兼容认证的响应字段和包装
- [x] 兼容登录的 refresh/access JWT、过期、登出和重新登录撤销
- [ ] 迁移现有 Rust/Redis 会话，登出时主动断开已建立的 WebSocket
- [x] 房间进出协议兼容
  - [x] join 的 `status/data/room/languages/user/rtcToken/startTime`，RTC token 仍为空占位
  - [x] leave 的 `status` 响应
  - [x] Rust 错误状态码与旧 Go HTTP 状态并存
- [ ] relay 设备协议兼容
  - [x] `devicerelayer` 首帧
  - [x] 基础 DeviceCommand 帧解析和回传
  - [ ] H5 和设备端互通、真实设备控制与状态广播

## 直播核心后续

- [ ] 完整 Rust RoomInfo 字段来源
  - [x] 每场独立 `liveId`、主播 `anchorNid` 和场次起止时间
  - [ ] RTC token 签发、观众 RTC UID、languages
  - [ ] banner、tag、description、plan、gifts、trans 等设置
- [ ] Rust 房间错误码和 DataNotFound 响应
- [ ] Rust 特有的房间在线状态及状态转换
- [ ] relay ticket 的 Rust 签名、TTL 和重连语义
- [ ] H5 relay 的生命周期消息、控制消息和完整 devicerelayer 命令

## 账号和用户

- [x] `/api/user/{user_id}`、`/api/user/me`
- [x] 用户语言设置和资料语言字段
- [x] 用户状态检查和用户应用列表基础响应
- [ ] 用户状态、头像和完整资料字段
- [x] 修改密码
- [ ] 密码重置、邮箱验证码
- [ ] 第三方登录和 token exchange

## 业务模块

- [x] 只读礼物目录 `GET /api/gift/list`
- [ ] 发送礼物、积分、收益、订阅和排行榜
- [ ] Feed、通知、私信和会话
- [ ] 文件上传、媒体审核和消息审核
- [ ] Agora、STT、翻译和设备控制
- [ ] 支付、钱包、订单和退款
- [ ] Anchor、MCN、NFT、装扮
- [ ] 完整后台管理、审计和客服机器人

## 基础设施和交付

- [ ] Redis 缓存和跨进程会话
- [x] 同一 PostgreSQL 数据库与 schema 的房间事件广播
- [x] Go 内部统计切换到独立 gRPC 监听端口，公开 HTTP 和 WebSocket 接口沿用原路径
- [ ] 与 Rust Zenoh 服务发现及旧 RPC 的互操作
- [ ] Kafka edge 消费链路
- [ ] etcd 配置和服务发现
- [ ] S3、邮件、推送和第三方服务适配
- [ ] Rust MySQL 数据导入 PostgreSQL
- [ ] 多实例、限流、Origin、心跳、监控和生产部署
- [ ] 原前端真实端到端验收

## 约束

- 继续兼容旧 Go 客户端使用的公开 HTTP 和 WebSocket 请求与响应。
- 需要改变 HTTP 状态或响应形状时，使用明确的协议选择方式。
- 不把用户、房间、消息 ID 转成 32 位。
- 不修改 CMake 或 prcmake。

## 外围业务分工

| 模块 | 先做的入口 | 依赖 |
| --- | --- | --- |
| 礼物 | 礼物目录、发送礼物、房间礼物事件 | 账号、房间、积分账本、EventBus |
| 订阅 | 查询主播方案、免费订阅/取消、订阅关系列表 | 用户、主播档案、订阅表 |
| Feed/通知 | 公开列表、我的通知、已读标记 | 账号、分页、通知表 |
| 私信 | 会话列表、消息分页、发送、已读 | 用户关系、消息表、审核钩子 |
| 支付 | 支付前校验、订单状态只读、外部支付代理边界 | 幂等订单、积分账本、外部支付配置 |
| 后台 | 用户、房间、礼物和订单的只读分页 | 管理权限、各业务 Store |
