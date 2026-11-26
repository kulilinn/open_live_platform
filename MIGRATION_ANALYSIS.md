# LiveMedia 后端移植说明

对照版本为 LiveMedia `4c9ee4358857cfd32a880c45fed959ad629bc4a1`。Go 入口是 `cmd/platform`。公开 HTTP、WebSocket、运营后台和 MCN 后台保留各自的方法、路径及监听边界。内部通信使用生成的 protobuf 和 gRPC。管理服务与业务服务部署在同一 Go 进程时直接调用同一组业务方法。

## 模块对应关系

| 原 Rust 服务 | Go 实现 | 负责的业务 |
| --- | --- | --- |
| `api` | `internal/httpapi` 和各业务包 | 用户、主播、房间、约会、礼物、订阅、帖子、私信、通知、钱包和支付 |
| `chat`、`agent` | `internal/live`、`internal/relay`、`internal/rtc` | 房间状态、直播场次、聊天室、设备控制、WebSocket 票据、RTC 身份 |
| `chatbot`、`edge` | `internal/livebot`、`internal/botchat`、`internal/edge` | 房间机器人、机器人私聊、Kafka 实时和延迟通知 |
| `admin`、`admin_mcn` | `internal/admin` | 运营、财务和机构账号权限，内容与主播审核，礼物配置，合约、结算及报表 |
| `service` | `api/platform/v1`、`internal/platformgrpc`、`internal/etcdlive` | 类型化的服务调用、健康检查、节点发现和配置更新 |
| `entity`、`cache`、`migration` | `internal/store/postgres`、`cmd/import-live-*`、`scripts` | 业务事务、跨实例状态、迁移及旧数据导入 |

## 运行和数据流

- `internal/app` 组装服务、注册路由、启动消费者和定时任务，统一管理监听端口与退出。单实例内存模式用于开发。多实例及财务和运营功能依赖 PostgreSQL。
- 房间变更、聊天及业务账目保存在 PostgreSQL。每个进程持有自己的 WebSocket 连接，通过 PostgreSQL 通知把房间事件或指定用户的消息投递到其他实例。RTC UID 单独分配，用户、房间和订单继续使用原业务编号。
- 礼物、付费订阅和财务操作先记录请求，再调用积分或支付服务。上游已确认的操作凭证可供重试及对账；结果未知的请求保留订单号，避免再扣一次。日报、机构分成和导入的历史账目在后台查询。
- 运行所需的外部适配器由配置启用，包括 Kafka、etcd、S3、SMTP、支付、Wallet 签名、身份核验、区块链只读查询及音视频服务。各自的环境变量、权限和启动命令见 [README.md](README.md)。
- 正常业务访问 PostgreSQL。迁移工具通过独立连接读取旧 MySQL 或 Redis，并处理老系统编号、密码哈希、过期时间与业务归属。账号、会话、历史业务、运营权限和部门分成有不同的切换语义，按 README 中对应命令操作。

## 兼容性核对

`scripts/audit_rust_backoffice_routes.py` 从原源码的路由挂载点展开嵌套前缀、HTTP 方法及通配符，并生成 `internal/app/testdata/livemedia_*_routes.txt`。Go 测试直接比较实际服务所挂载的方法和路由模板。一次匹配通用回退处理器会导致测试失败。

| 接口面 | 方法和路径组合 | 统计范围 |
| --- | ---: | --- |
| 公开客户端和服务回调 | 281 | 含 WebSocket 握手、支付通配和六个审核入口 |
| 受信用户服务和调度 | 9 | 账户查询、授权码交换及直播收入结算调度 |
| 运营及财务后台 | 146 | 143 个 `/api` 路径，其中支付同时接受 GET、POST，另有 2 个顶层入口 |
| MCN 后台 | 61 | 60 个 `/api` 入口和 1 个健康检查 |

Rust 独立服务之间的 `/api/internal` 转发属于旧的进程间实现。对应业务在 Go 管理监听器、业务服务或 gRPC 中处理。上述数量度量路由分派。事务回滚、租户权限、返回内容、账目精度、事件投递与并发行为由各业务用例另外检查。外部验收事项见 [TODO.md](TODO.md)。
