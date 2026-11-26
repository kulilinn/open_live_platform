# LiveMedia Go 移植任务清单

## 当前基线

- 仓库目录为 `D:\my\open_live_platform`。
- 当前主线已完成 Go 核心服务、PostgreSQL 数据层、房间聊天历史、H5 relay 聊天、房间状态查询和基础 `RoomInfo` 映射。
- 已验证 `go test ./...`、真实 PostgreSQL 回归、`go vet ./...` 和 Go 编译。
- 保留 64 位用户、房间、消息 ID。未修改任何 `CMakeLists.txt` 或 `prcmake` 文件。

## 本轮优先完成

- [ ] Rust 认证响应和会话语义兼容
  - [ ] signin 的 refresh/access 会话转换
  - [x] signup、signin 的 Rust 字段和包装
  - [ ] session、signout 的协议模式选择
- [x] 房间进出协议兼容
  - [x] join 的 `status/data/room/languages/user/rtcToken/startTime`，RTC token 仍为空占位
  - [x] leave 的 `status` 响应
  - [x] Rust 错误状态码与旧 Go HTTP 状态并存
- [x] relay 设备协议兼容
  - [x] `devicerelayer` 首帧
  - [x] DeviceCommand 的 heartbeat、devices、connector 状态
  - [x] 设备消息转发和最小测试

## 直播核心后续

- [ ] 完整 Rust RoomInfo 字段来源
  - [ ] 每场独立 `liveId`
  - [ ] `anchorNid`、RTC token、languages
  - [ ] banner、tag、description、plan、gifts、trans 等设置
- [ ] Rust 房间错误码和 DataNotFound 响应
- [ ] 房间在线状态、直播状态和时长的持久化
- [ ] relay ticket 的 Rust 签名、TTL 和重连语义
- [ ] H5 relay 的生命周期消息、控制消息和完整 devicerelayer 命令

## 账号和用户

- [ ] `/api/user/{user_id}`、`/api/user/me`
- [ ] 用户语言、状态、资料和头像字段
- [ ] 密码重置、修改密码、邮箱验证码
- [ ] 第三方登录和 token exchange

## 业务模块

- [ ] 礼物、积分、收益、订阅和排行榜
- [ ] Feed、通知、私信和会话
- [ ] 文件上传、媒体审核和消息审核
- [ ] Agora、STT、翻译和设备控制
- [ ] 支付、钱包、订单和退款
- [ ] Anchor、MCN、NFT、装扮
- [ ] 完整后台管理、审计和客服机器人

## 基础设施和交付

- [ ] Redis 缓存和跨进程会话
- [ ] Zenoh/RPC 多实例广播
- [ ] Kafka edge 消费链路
- [ ] etcd 配置和服务发现
- [ ] S3、邮件、推送和第三方服务适配
- [ ] Rust MySQL 数据导入 PostgreSQL
- [ ] 多实例、限流、Origin、心跳、监控和生产部署
- [ ] 原前端真实端到端验收

## 约束

- 继续保留旧 Go 客户端可用的请求和响应路径。
- 需要改变 HTTP 状态或响应形状时，使用明确的协议选择方式。
- 不把用户、房间、消息 ID 转成 32 位。
- 不修改 CMake 或 prcmake。
