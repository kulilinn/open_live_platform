package app

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"open_live_platform/internal/admin"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/date"
	"open_live_platform/internal/dm"
	"open_live_platform/internal/edge"
	"open_live_platform/internal/feed"
	"open_live_platform/internal/gift"
	"open_live_platform/internal/httpapi"
	"open_live_platform/internal/live"
	"open_live_platform/internal/notice"
	"open_live_platform/internal/payment"
	"open_live_platform/internal/relay"
	"open_live_platform/internal/rtc"
	"open_live_platform/internal/store/postgres"
	"open_live_platform/internal/stt"
	"open_live_platform/internal/subscription"
	"open_live_platform/internal/translate"
	"open_live_platform/internal/upload"
)

type Config struct {
	Google                 auth.GoogleConfig
	Upload                 upload.Config
	WalletPasses           upload.PassConfig
	Kafka                  edge.KafkaConfig
	DatabaseURL            string
	Addr                   string
	GRPCAddr               string
	AdminToken             string
	AdminAddr              string
	MCNAdminAddr           string
	AdminAccountsFile      string
	EdgeToken              string
	RelaySecret            string
	RelayPublicURL         string
	RelayRust              relay.RustOptions
	GiftCatalogFile        string
	JWTSecret              string
	MailSMTP               auth.SMTPConfig
	MailCodeSecret         string
	MailCodeTTL            time.Duration
	AgoraAppID             string
	AgoraCertificate       string
	AgoraTokenExpire       string
	TextTranslation        translate.Config
	Subtitles              stt.Config
	DMModerationURL        string
	DMModerator            dm.Moderator
	PaymentHost            string
	SubscriptionRenewToken string
	PaymentAppID           string
	PaymentBorrowAppID     string
	PaymentProxyRoutes     string
	LargeOrders            payment.LargeOrderConfig

	AnchorAwayTimeout time.Duration
	ReadTimeout       time.Duration
	ReadHeaderTimeout time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
}

func ConfigFromEnv() Config {
	adminToken := os.Getenv("ADMIN_TOKEN")
	return Config{
		Google:                 auth.GoogleConfig{ClientID: os.Getenv("GOOGLE_CLIENT_ID")},
		Upload:                 uploadFromEnv(),
		WalletPasses:           walletPassesFromEnv(),
		Kafka:                  kafkaFromEnv(),
		DatabaseURL:            os.Getenv("DATABASE_URL"),
		Addr:                   env("PLATFORM_ADDR", ":8080"),
		GRPCAddr:               os.Getenv("PLATFORM_GRPC_ADDR"),
		AdminToken:             adminToken,
		AdminAddr:              os.Getenv("PLATFORM_ADMIN_ADDR"),
		MCNAdminAddr:           os.Getenv("PLATFORM_MCN_ADMIN_ADDR"),
		AdminAccountsFile:      os.Getenv("ADMIN_ACCOUNTS_FILE"),
		EdgeToken:              env("EDGE_TOKEN", adminToken),
		RelaySecret:            env("RELAY_SECRET", "dev-secret"),
		RelayPublicURL:         os.Getenv("RELAY_PUBLIC_URL"),
		RelayRust:              relayRustOptionsFromEnv(),
		GiftCatalogFile:        os.Getenv("GIFT_CATALOG_FILE"),
		JWTSecret:              os.Getenv("JWT_SECRET"),
		MailSMTP:               smtpFromEnv(),
		MailCodeSecret:         os.Getenv("MAIL_CODE_SECRET"),
		MailCodeTTL:            mailCodeTTLFromEnv(),
		AgoraAppID:             os.Getenv("AGORA_APP_ID"),
		AgoraCertificate:       os.Getenv("AGORA_APP_CERTIFICATE"),
		AgoraTokenExpire:       os.Getenv("AGORA_TOKEN_EXPIRE"),
		TextTranslation:        translate.ConfigFromEnv(),
		Subtitles:              stt.ConfigFromEnv(),
		DMModerationURL:        os.Getenv("DM_MODERATION_URL"),
		PaymentHost:            os.Getenv("PAYMENT_HOST"),
		SubscriptionRenewToken: os.Getenv("SUBSCRIPTION_RENEW_TOKEN"),
		PaymentAppID:           os.Getenv("PAYMENT_APPID"),
		PaymentBorrowAppID:     os.Getenv("PAYMENT_BORROW_APPID"),
		PaymentProxyRoutes:     os.Getenv("PAYMENT_PROXY_ROUTES"),
		LargeOrders:            largeOrderFromEnv(),
		AnchorAwayTimeout:      anchorAwayTimeoutFromEnv(),
		ReadTimeout:            duration("PLATFORM_READ_TIMEOUT", 15*time.Second),
		ReadHeaderTimeout:      duration("PLATFORM_READ_HEADER_TIMEOUT", 5*time.Second),
		WriteTimeout:           duration("PLATFORM_WRITE_TIMEOUT", 15*time.Second),
		IdleTimeout:            duration("PLATFORM_IDLE_TIMEOUT", 60*time.Second),
	}
}

type App struct {
	push          relay.PushPrivate
	dates         *httpapi.Dates
	Config        Config
	Accounts      auth.AccountStore
	Rooms         live.RoomStore
	DM            dm.Store
	Subtitles     *stt.Service
	Server        *http.Server
	Backoffice    *http.Server
	MCNBackoffice *http.Server
	db            *postgres.Store
}

func New(ctx context.Context, config Config) (*App, error) {
	config.GRPCAddr = cmp.Or(config.GRPCAddr, "127.0.0.1:9090")
	if config.AnchorAwayTimeout <= 0 {
		config.AnchorAwayTimeout = live.DefaultAnchorAwayTimeout
	}
	signer, err := rtc.NewSigner(config.AgoraAppID, config.AgoraCertificate, config.AgoraTokenExpire)
	if err != nil {
		return nil, err
	}
	config.Subtitles.AppID = config.AgoraAppID
	if config.Subtitles.Version != "" && config.Subtitles.Version != stt.Legacy && config.Subtitles.Version != stt.Agent {
		return nil, fmt.Errorf("AGORA_STT_API_VERSION must be v1 or v2")
	}
	mailSender, err := auth.NewSMTPSender(config.MailSMTP)
	if err != nil {
		return nil, err
	}
	gifts, err := gift.Load(config.GiftCatalogFile)
	if err != nil {
		return nil, fmt.Errorf("gift catalog: %w", err)
	}
	if config.DMModerator == nil && config.DMModerationURL != "" {
		moderator, err := dm.NewHTTPModerator(config.DMModerationURL)
		if err != nil {
			return nil, err
		}
		config.DMModerator = moderator
	}
	billing, err := payment.NewClient(config.PaymentHost, config.PaymentAppID)
	if err != nil {
		return nil, err
	}
	paymentProxy, err := payment.NewProxy(billing, config.PaymentBorrowAppID, config.PaymentProxyRoutes)
	if err != nil {
		return nil, err
	}
	largeOrders, err := payment.NewLargeOrderService(billing, config.LargeOrders)
	if err != nil {
		return nil, err
	}
	passes, err := upload.NewPasses(config.WalletPasses)
	if err != nil {
		return nil, err
	}
	var principals admin.Principals
	if config.AdminAddr != "" || config.MCNAdminAddr != "" {
		if config.DatabaseURL == "" || config.AdminAccountsFile == "" {
			return nil, fmt.Errorf("management listeners require DATABASE_URL and ADMIN_ACCOUNTS_FILE")
		}
		principals, err = admin.ReadPrincipals(config.AdminAccountsFile)
		if err != nil {
			return nil, err
		}
	}
	var backend auth.JWTBackend
	var rooms live.RoomStore
	var notices notice.Store
	var uploads upload.Store
	var inbox feed.Store
	var messages dm.Store
	var giftOrders gift.OrderStore
	var payments payment.Store
	var subscriptions subscription.Store
	var db *postgres.Store
	if config.DatabaseURL == "" {
		memoryRooms := live.NewStoreWithAnchorTimeout(config.AnchorAwayTimeout)
		backend, rooms, giftOrders = auth.NewStore(), memoryRooms, memoryRooms
		notices = &notice.Memory{}
		uploads = &upload.Memory{}
		inbox = &feed.Memory{}
		memoryMessages := &dm.Memory{}
		messages = memoryMessages
		subscriptions = subscription.NewMemory(memoryMessages)
		payments = &payment.MemoryChecks{}
	} else {
		var err error
		db, err = postgres.OpenWithAnchorTimeout(ctx, config.DatabaseURL, config.AnchorAwayTimeout)
		if err != nil {
			return nil, err
		}
		backend, rooms = db, db
		config.RelayRust.Tickets = db
		config.RelayRust.Devices = db
		config.RelayRust.Grants = db
		notices = db
		uploads = db
		inbox = db
		messages = db
		giftOrders = db
		subscriptions = db
		payments = db
	}
	accounts := auth.WithJWT(backend, config.JWTSecret)
	emailVerification, err := auth.NewEmailVerification(accounts, mailSender, cmp.Or(config.MailCodeSecret, config.JWTSecret), config.MailCodeTTL)
	if err != nil {
		if db != nil {
			db.Shutdown()
		}
		return nil, err
	}
	backoffice, err := managementServer(config, db, principals, config.AdminAddr, admin.OperationArea, gifts)
	if err != nil {
		db.Shutdown()
		return nil, err
	}
	mcnBackoffice, err := managementServer(config, db, principals, config.MCNAdminAddr, admin.MCNArea, nil)
	if err != nil {
		db.Shutdown()
		return nil, err
	}
	mux := http.NewServeMux()
	var taskStore stt.Store
	var dateStore date.Store
	if db != nil {
		taskStore = db
		dateStore = db
	} else {
		dateStore = date.NewMemory()
	}
	subtitles := stt.New(config.Subtitles, signer, rooms, taskStore)
	dateSubtitles := stt.New(config.Subtitles, signer, rooms, dateStore.DateSubtitles())
	push := relay.Register(mux, accounts, rooms, config.RelaySecret, config.RelayPublicURL, config.RelayRust)
	dates := &httpapi.Dates{Accounts: accounts, Service: date.New(dateStore, accounts, subscriptions, billing),
		Signer: signer, Subtitles: dateSubtitles, Push: push}
	dates.Register(mux)

	httpapi.RegisterWithEmail(mux, accounts, rooms, signer, emailVerification, httpapi.LiveServices{Subtitles: subtitles, Dates: dates})
	httpapi.RegisterSTT(mux, accounts, subtitles, dateSubtitles)
	httpapi.RegisterTranslate(mux, accounts, translate.New(config.TextTranslation))
	httpapi.RegisterGoogle(mux, auth.GoogleLogin{Config: config.Google, Accounts: accounts, Backend: backend, Mail: emailVerification})
	httpapi.RegisterGiftCatalog(mux, accounts, gifts)
	httpapi.RegisterPoints(mux, accounts, billing)
	httpapi.RegisterPayment(mux, accounts, payments, paymentProxy, largeOrders)
	httpapi.RegisterWalletPass(mux, passes)
	httpapi.RegisterGiftOrders(mux, accounts, rooms, giftOrders, billing, gifts)
	httpapi.RegisterSubscriptions(mux, accounts, rooms, subscriptions, billing, inbox, config.SubscriptionRenewToken)
	admin.Register(mux, accounts, rooms, config.AdminToken)
	httpapi.RegisterNotices(mux, accounts, notices)
	httpapi.RegisterFeed(mux, accounts, rooms, subscriptions, inbox, messages, config.DMModerator)
	httpapi.RegisterUploads(mux, accounts, config.Upload, uploads)
	httpapi.RegisterDirectMessages(mux, accounts, messages, config.DMModerator, push)
	edge.Register(mux, rooms, notices, config.EdgeToken)

	return &App{
		push:          push,
		dates:         dates,
		db:            db,
		Config:        config,
		Accounts:      accounts,
		Rooms:         rooms,
		DM:            messages,
		Backoffice:    backoffice,
		MCNBackoffice: mcnBackoffice,
		Subtitles:     subtitles,
		Server: &http.Server{
			Addr:              config.Addr,
			Handler:           mux,
			ReadTimeout:       config.ReadTimeout,
			ReadHeaderTimeout: config.ReadHeaderTimeout,
			WriteTimeout:      config.WriteTimeout,
			IdleTimeout:       config.IdleTimeout,
		},
	}, nil
}

func managementServer(config Config, db *postgres.Store, principals admin.Principals, address, area string, catalog []gift.Gift) (*http.Server, error) {
	if address == "" {
		return nil, nil
	}
	handler, err := admin.BackofficeHandler(area, principals, db, catalog)
	if err != nil {
		return nil, err
	}
	return &http.Server{Addr: address, Handler: handler, ReadTimeout: config.ReadTimeout,
		ReadHeaderTimeout: config.ReadHeaderTimeout, WriteTimeout: config.WriteTimeout, IdleTimeout: config.IdleTimeout}, nil
}

func largeOrderFromEnv() payment.LargeOrderConfig {
	fromAddress, _, _ := strings.Cut(os.Getenv("LOVEROSE_PUSH_EMAIL"), ";")
	fromName, _, _ := strings.Cut(os.Getenv("LOVEROSE_PUSH_EMAIL_NAME"), ";")
	host := os.Getenv("LOVEROSE_PUSH_HOST")
	return payment.LargeOrderConfig{
		Region: os.Getenv("PAYMENT_LARGE_ORDER_REGION"), NotificationURL: host + os.Getenv("LOVEROSE_PUSH_PATH"),
		FromAddress: strings.TrimSpace(fromAddress), FromName: strings.TrimSpace(fromName),
		TemplateCN: os.Getenv("PAYMENT_LARGE_ORDER_TEMPLATE_ZH_CN"), TemplateTW: os.Getenv("PAYMENT_LARGE_ORDER_TEMPLATE_ZH_TW"),
		TemplateJP: os.Getenv("PAYMENT_LARGE_ORDER_TEMPLATE_JA_JP"), TemplateEN: os.Getenv("PAYMENT_LARGE_ORDER_TEMPLATE_EN_US"),
	}
}

func kafkaFromEnv() edge.KafkaConfig {
	var cfg edge.KafkaConfig
	if brokers := os.Getenv("KAFKA_BROKERS"); brokers != "" {
		cfg.Brokers = strings.FieldsFunc(brokers, func(r rune) bool { return r == ',' || r == ';' || r == ' ' })
		cfg.TLS, _ = strconv.ParseBool(os.Getenv("KAFKA_SSL"))
		for i, prefix := range []string{"REALTIME_NOTICE", "DELAY_NOTICE"} {
			topic, group := os.Getenv(prefix+"_TOPIC"), os.Getenv(prefix+"_GROUP")
			if topic == "" && group == "" {
				continue
			}
			seconds, err := strconv.ParseUint(os.Getenv(prefix+"_TTL"), 10, 32)
			if err != nil {
				seconds = uint64(i+1) * 3600
			}
			cfg.Topics = append(cfg.Topics, edge.NoticeTopic{Name: topic, Group: group, TTL: time.Duration(seconds) * time.Second})
		}
	}
	return cfg
}

func uploadFromEnv() upload.Config {
	size := func(name string) int64 {
		n, _ := strconv.ParseUint(os.Getenv(name), 10, 32)
		return int64(n)
	}
	return upload.Config{Bucket: os.Getenv("AWS_S3_BUCKET"), Region: os.Getenv("AWS_S3_REGION"),
		Endpoint: os.Getenv("AWS_S3_ENDPOINT_URL"), KeyID: os.Getenv("AWS_S3_ACCESS_KEY_ID"), Secret: os.Getenv("AWS_S3_SECRET_ACCESS_KEY"),
		Parent: os.Getenv("AWS_S3_PARENT_PATH"), BaseURL: os.Getenv("BASE_STATIC_URL"),
		ChunkSize: size("UPLOAD_CHUNK_SIZE"), MaxImage: size("MAX_IMAGE_UPLOAD_SIZE"), MaxVideo: size("MAX_VIDEO_UPLOAD_SIZE")}
}

func walletPassesFromEnv() upload.PassConfig {
	storage := uploadFromEnv()
	storage.Bucket = os.Getenv("AWS_PASSES_S3_BUCKET")
	return upload.PassConfig{IndexFile: os.Getenv("WALLET_PASS_INDEX_FILE"), AppName: os.Getenv("APP_NAME"), Storage: storage}
}

func smtpFromEnv() auth.SMTPConfig {
	address := os.Getenv("MAIL_SMTP_ADDR")
	from := os.Getenv("MAIL_SMTP_FROM")
	if address != "" {
		from = cmp.Or(from, os.Getenv("LR_FROM_MAIL"))
	}
	return auth.SMTPConfig{
		Address: address, From: from, Username: os.Getenv("MAIL_SMTP_USERNAME"), Password: os.Getenv("MAIL_SMTP_PASSWORD"),
	}
}

func mailCodeTTLFromEnv() time.Duration {
	seconds, err := strconv.ParseUint(os.Getenv("MAIL_CODE_EXPIRY"), 10, 32)
	if err != nil || seconds == 0 {
		return time.Minute
	}
	return time.Duration(seconds) * time.Second
}

func (a *App) Close() {
	if a.db != nil {
		a.db.Shutdown()
	}
}

func anchorAwayTimeoutFromEnv() time.Duration {
	seconds, err := strconv.ParseUint(os.Getenv("TEMP_OFFLINE_TIMEOUT"), 10, 32)
	if err != nil || seconds == 0 {
		return live.DefaultAnchorAwayTimeout
	}
	return time.Duration(seconds) * time.Second
}

func relayRustOptionsFromEnv() relay.RustOptions {
	seconds, _ := strconv.ParseUint(os.Getenv("WS_TOEKN_EXPIRE"), 10, 32)
	appID, _ := strconv.ParseInt(os.Getenv("RELAY_APP_ID"), 10, 64)
	var users []int64
	for _, field := range strings.Split(os.Getenv("RELAY_APP_USERS"), ",") {
		if uid, err := strconv.ParseInt(strings.TrimSpace(field), 10, 64); err == nil && uid > 0 {
			users = append(users, uid)
		}
	}
	return relay.RustOptions{AppID: appID, AppKey: os.Getenv("RELAY_APP_KEY"), AppSecret: os.Getenv("RELAY_APP_SECRET"), AppUsers: users, TTL: time.Duration(seconds) * time.Second}
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func duration(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}
