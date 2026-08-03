package logx

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type idKey struct{}

func TestParseLevel(t *testing.T) {
	ok := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		"INFO":    slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
	}
	for name, want := range ok {
		got, err := parseLevel(name)
		if err != nil {
			t.Errorf("parseLevel(%q): %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("parseLevel(%q) = %v, want %v", name, got, want)
		}
	}

	// 级别写错要报错，不能静默降级成 info
	if _, err := parseLevel("verbose"); err == nil {
		t.Error("parseLevel(\"verbose\") should return an error")
	}
}

func TestInitRejectsBadConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{"bad level", Config{Level: "trace", Mode: ModeDev}},
		{"bad mode", Config{Level: "info", Mode: "staging"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := Init(tt.cfg); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestInitDev(t *testing.T) {
	if err := Init(Config{Mode: ModeDev, Level: "debug"}); err != nil {
		t.Fatalf("Init: %v", err)
	}
}

// prod 模式：app.log 收全量，error.log 只收 ERROR
func TestInitProdLevelSplit(t *testing.T) {
	dir := t.TempDir()
	if err := Init(Config{Mode: ModeProd, Level: "info", Dir: dir}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	slog.Info("info line")
	slog.Error("error line")

	app := readFile(t, filepath.Join(dir, "app.log"))
	if !strings.Contains(app, "info line") {
		t.Error("app.log should contain the info line")
	}
	if !strings.Contains(app, "error line") {
		t.Error("app.log should contain the error line too")
	}

	errLog := readFile(t, filepath.Join(dir, "error.log"))
	if strings.Contains(errLog, "info line") {
		t.Error("error.log should not contain the info line")
	}
	if !strings.Contains(errLog, "error line") {
		t.Error("error.log should contain the error line")
	}
}

func TestInitProdSplitByHost(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOSTNAME", "pod-7")

	if err := Init(Config{Mode: ModeProd, Level: "info", Dir: dir, SplitByHost: true}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	slog.Info("hello")

	if _, err := os.Stat(filepath.Join(dir, "pod-7", "app.log")); err != nil {
		t.Fatalf("expected log under HOSTNAME subdir: %v", err)
	}
}

func TestInitProdSplitByHostFallsBackToUnknown(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOSTNAME", "")

	if err := Init(Config{Mode: ModeProd, Level: "info", Dir: dir, SplitByHost: true}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	slog.Info("hello")

	if _, err := os.Stat(filepath.Join(dir, "unknown", "app.log")); err != nil {
		t.Fatalf("expected fallback to unknown/: %v", err)
	}
}

func TestWithDefaults(t *testing.T) {
	cfg := withDefaults(Config{})
	if cfg.Level != "info" || cfg.Mode != ModeProd || cfg.Dir != "logs" {
		t.Errorf("unexpected defaults: %+v", cfg)
	}

	// 轮转三项的 0 在 lumberjack 那边是"不删旧日志"，不能被当成"没配"顶成别的值。
	// nebula 的 config.yaml 就是靠 max_age: 0 / max_backups: 0 保留全量日志的。
	if cfg.MaxSize != 0 || cfg.MaxAge != 0 || cfg.MaxBackups != 0 {
		t.Errorf("rotation zero values must pass through untouched: %+v", cfg)
	}

	// 已填的值不该被覆盖
	given := Config{Level: "debug", Mode: ModeDev, Dir: "/tmp/l", MaxSize: 1, MaxAge: 2, MaxBackups: 3}
	if got := withDefaults(given); got != given {
		t.Errorf("withDefaults changed an explicit config: %+v", got)
	}
}

func TestTraceHandlerInjectsAttrs(t *testing.T) {
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, nil)

	requestID := func(ctx context.Context) []slog.Attr {
		if id, ok := ctx.Value(idKey{}).(string); ok {
			return []slog.Attr{slog.String("requestId", id)}
		}
		return nil
	}
	taskID := func(context.Context) []slog.Attr {
		return []slog.Attr{slog.String("taskId", "t-1")}
	}

	logger := slog.New(newTraceHandler(base, []Extractor{requestID, taskID}))
	logger.InfoContext(context.WithValue(context.Background(), idKey{}, "r-1"), "hello")

	out := buf.String()
	if !strings.Contains(out, `"requestId":"r-1"`) {
		t.Errorf("missing requestId in %s", out)
	}
	if !strings.Contains(out, `"taskId":"t-1"`) {
		t.Errorf("missing taskId in %s", out)
	}
}

// 没有 extractor 时不该多包一层
func TestNewTraceHandlerNoExtractors(t *testing.T) {
	base := slog.NewJSONHandler(&bytes.Buffer{}, nil)
	if got := newTraceHandler(base, nil); got != slog.Handler(base) {
		t.Error("newTraceHandler should return base unchanged when there are no extractors")
	}
}

func TestTraceHandlerKeepsAttrsAndGroups(t *testing.T) {
	var buf bytes.Buffer
	extract := func(context.Context) []slog.Attr {
		return []slog.Attr{slog.String("requestId", "r-1")}
	}
	h := newTraceHandler(slog.NewJSONHandler(&buf, nil), []Extractor{extract})

	slog.New(h).With("service", "svc").WithGroup("g").Info("hello", "k", "v")

	out := buf.String()
	for _, want := range []string{`"requestId":"r-1"`, `"service":"svc"`, `"g":{"k":"v"}`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in %s", want, out)
		}
	}
}

// 从同一个父 logger 分叉出的两个 logger 不能互相串字段
func TestTraceHandlerSiblingsDoNotLeak(t *testing.T) {
	var buf bytes.Buffer
	extract := func(context.Context) []slog.Attr {
		return []slog.Attr{slog.String("requestId", "r-1")}
	}
	parent := slog.New(newTraceHandler(slog.NewJSONHandler(&buf, nil), []Extractor{extract}))

	parent.WithGroup("a").Info("first")
	firstLine := buf.String()
	buf.Reset()
	parent.WithGroup("b").Info("second")
	secondLine := buf.String()

	if strings.Contains(firstLine, `"b"`) {
		t.Errorf("first logger leaked group b: %s", firstLine)
	}
	if strings.Contains(secondLine, `"a"`) {
		t.Errorf("second logger leaked group a: %s", secondLine)
	}
	for _, line := range []string{firstLine, secondLine} {
		if !strings.Contains(line, `"requestId":"r-1"`) {
			t.Errorf("requestId should stay at top level: %s", line)
		}
	}
}

func TestMultiHandlerLevelSplit(t *testing.T) {
	var all, errsOnly bytes.Buffer
	h := newMultiHandler(
		slog.NewJSONHandler(&all, &slog.HandlerOptions{Level: slog.LevelInfo}),
		slog.NewJSONHandler(&errsOnly, &slog.HandlerOptions{Level: slog.LevelError}),
	)
	logger := slog.New(h)
	logger.Info("info line")
	logger.Error("error line")

	if !strings.Contains(all.String(), "info line") || !strings.Contains(all.String(), "error line") {
		t.Errorf("info handler should see both lines: %s", all.String())
	}
	if strings.Contains(errsOnly.String(), "info line") {
		t.Errorf("error handler should not see the info line: %s", errsOnly.String())
	}
	if !strings.Contains(errsOnly.String(), "error line") {
		t.Errorf("error handler should see the error line: %s", errsOnly.String())
	}
}

func TestMultiHandlerEnabled(t *testing.T) {
	h := newMultiHandler(
		slog.NewJSONHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelError}),
		slog.NewJSONHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelDebug}),
	)
	// 任一 handler 收就算开启
	if !h.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("Enabled(debug) = false, want true")
	}
}

// 广播给多个 handler 时 Record 不能互相污染
func TestMultiHandlerDoesNotShareRecord(t *testing.T) {
	var a, b bytes.Buffer
	h := newMultiHandler(slog.NewJSONHandler(&a, nil), slog.NewJSONHandler(&b, nil))
	slog.New(h).Info("hello", "k", "v")

	if a.String() == "" || b.String() == "" {
		t.Fatal("both handlers should receive output")
	}
	if strings.Count(a.String(), `"k":"v"`) != 1 {
		t.Errorf("attr duplicated in first handler: %s", a.String())
	}
	if strings.Count(b.String(), `"k":"v"`) != 1 {
		t.Errorf("attr duplicated in second handler: %s", b.String())
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}
