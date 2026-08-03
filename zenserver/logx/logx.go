// Package logx 配置 log/slog 的全局默认 logger：级别、dev/prod 格式、文件轮转，
// 以及从 context 提取追踪字段。
//
//	if err := logx.Init(cfg.Log, ginx.TraceAttrs); err != nil {
//	    return err
//	}
//	slog.InfoContext(ctx, "server started")   // 自动带上 request id
//
// dev 模式输出纯文本到 stdout。prod 模式输出 JSON 并做级别分流：app.log 收 Level
// 以上的全量日志，error.log 与 stdout 只收 ERROR，容器里方便直接排查。
//
// Init 修改的是 slog 的全局默认 logger，一个进程只应调用一次。
package logx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/natefinch/lumberjack.v2"
)

// 日志模式
const (
	ModeDev  = "dev"  // 纯文本输出到 stdout，不分流
	ModeProd = "prod" // JSON 输出到文件，按级别分流
)

// Config 日志配置，可直接嵌进使用方自己的配置结构体，yaml 与 env tag 都已备好。
//
// 轮转相关的三个字段原样交给 lumberjack，零值用它自己的语义，本包不做加工——
// 0 在那边是有意义的取值（不删旧日志），不能当成"没配"处理。
type Config struct {
	Level       string `yaml:"level" env:"LOG_LEVEL"`                 // debug|info|warn|error，默认 info
	Mode        string `yaml:"mode" env:"LOG_MODE"`                   // dev|prod，默认 prod
	Dir         string `yaml:"dir" env:"LOG_DIR"`                     // prod 日志目录，默认 logs
	MaxSize     int    `yaml:"max_size" env:"LOG_MAX_SIZE"`           // 单文件 MB，0 = lumberjack 默认的 100
	MaxAge      int    `yaml:"max_age" env:"LOG_MAX_AGE"`             // 保留天数，0 = 不按时间删除
	MaxBackups  int    `yaml:"max_backups" env:"LOG_MAX_BACKUPS"`     // 保留个数，0 = 全部保留
	Compress    bool   `yaml:"compress" env:"LOG_COMPRESS"`           // 轮转后是否压缩
	SplitByHost bool   `yaml:"split_by_host" env:"LOG_SPLIT_BY_HOST"` // 按 HOSTNAME 建子目录，k8s 多 pod 共享卷时开
}

// Extractor 从 context 提取要附加到每条日志的字段，无字段时返回 nil。
//
// 库不认识任何具体的追踪 key：request id 由 ginx.TraceAttrs 提供，
// 项目自己的 task id 之类由项目自己提供。
type Extractor func(ctx context.Context) []slog.Attr

// Init 按 cfg 配置 slog 的全局默认 logger。
// extractors 会在每条日志记录时依次调用，用于注入 request id 等追踪字段。
func Init(cfg Config, extractors ...Extractor) error {
	cfg = withDefaults(cfg)

	level, err := parseLevel(cfg.Level)
	if err != nil {
		return err
	}

	var base slog.Handler
	switch cfg.Mode {
	case ModeDev:
		base = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	case ModeProd:
		if base, err = newRotateHandler(cfg, level); err != nil {
			return err
		}
	default:
		return fmt.Errorf("logx: unknown mode %q, want %s|%s", cfg.Mode, ModeDev, ModeProd)
	}

	slog.SetDefault(slog.New(newTraceHandler(base, extractors)))
	return nil
}

// newRotateHandler 构造 prod 模式的分流 handler
func newRotateHandler(cfg Config, level slog.Level) (slog.Handler, error) {
	dir := cfg.Dir
	if cfg.SplitByHost {
		host := os.Getenv("HOSTNAME")
		if host == "" {
			host = "unknown"
		}
		dir = filepath.Join(dir, host)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("logx: create log dir %s: %w", dir, err)
	}

	rotate := func(name string) io.Writer {
		return &lumberjack.Logger{
			Filename:   filepath.Join(dir, name),
			MaxSize:    cfg.MaxSize,
			MaxAge:     cfg.MaxAge,
			MaxBackups: cfg.MaxBackups,
			Compress:   cfg.Compress,
		}
	}

	app := slog.NewJSONHandler(rotate("app.log"), &slog.HandlerOptions{Level: level})
	errs := slog.NewJSONHandler(
		io.MultiWriter(os.Stdout, rotate("error.log")),
		&slog.HandlerOptions{Level: slog.LevelError},
	)
	return newMultiHandler(app, errs), nil
}

// withDefaults 填充零值字段的默认值。
// 只处理 Level/Mode/Dir——轮转三项的零值在 lumberjack 那边有明确含义，不能覆盖。
func withDefaults(cfg Config) Config {
	if cfg.Level == "" {
		cfg.Level = "info"
	}
	if cfg.Mode == "" {
		cfg.Mode = ModeProd
	}
	if cfg.Dir == "" {
		cfg.Dir = "logs"
	}
	return cfg
}

// parseLevel 解析级别名，写错直接报错而不是降级成 info——配错的时候正是最需要知道的时候
func parseLevel(name string) (slog.Level, error) {
	switch strings.ToLower(name) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("logx: unknown level %q, want debug|info|warn|error", name)
	}
}
