package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type dbConf struct {
	Host string `yaml:"host" env:"TEST_DB_HOST"`
	Port int    `yaml:"port" env:"TEST_DB_PORT"`
}

type optional struct {
	Enabled bool `yaml:"enabled" env:"TEST_OPT_ENABLED"`
}

type testConf struct {
	Name     string        `yaml:"name" env:"TEST_NAME"`
	Timeout  time.Duration `yaml:"timeout" env:"TEST_TIMEOUT"`
	Ratio    float64       `yaml:"ratio" env:"TEST_RATIO"`
	Count    uint16        `yaml:"count" env:"TEST_COUNT"`
	Debug    bool          `yaml:"debug" env:"TEST_DEBUG"`
	NoTag    string        `yaml:"no_tag"`
	DB       dbConf        `yaml:"db"`
	Optional *optional     `yaml:"optional"`
	hidden   string
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// 默认值 → yaml → 环境变量，三层覆盖顺序
func TestLoadIntoOverrideOrder(t *testing.T) {
	path := writeConfig(t, "name: from-yaml\ndb:\n  host: yaml-host\n  port: 3306\n")
	t.Setenv("TEST_DB_HOST", "env-host")

	cfg := &testConf{
		Name:  "from-default",
		Ratio: 1.5,
		NoTag: "untouched",
		DB:    dbConf{Host: "default-host", Port: 1},
	}
	if err := LoadInto(path, cfg); err != nil {
		t.Fatalf("LoadInto: %v", err)
	}

	if cfg.Name != "from-yaml" {
		t.Errorf("yaml should override default: Name = %q, want from-yaml", cfg.Name)
	}
	if cfg.Ratio != 1.5 {
		t.Errorf("default should survive when yaml omits the key: Ratio = %v, want 1.5", cfg.Ratio)
	}
	if cfg.NoTag != "untouched" {
		t.Errorf("NoTag = %q, want untouched", cfg.NoTag)
	}
	if cfg.DB.Host != "env-host" {
		t.Errorf("env should override yaml: DB.Host = %q, want env-host", cfg.DB.Host)
	}
	if cfg.DB.Port != 3306 {
		t.Errorf("yaml should win when env is unset: DB.Port = %d, want 3306", cfg.DB.Port)
	}
}

func TestOverrideFromEnvTypes(t *testing.T) {
	t.Setenv("TEST_NAME", "n")
	t.Setenv("TEST_TIMEOUT", "1m30s")
	t.Setenv("TEST_RATIO", "0.25")
	t.Setenv("TEST_COUNT", "65535")
	t.Setenv("TEST_DEBUG", "true")
	t.Setenv("TEST_OPT_ENABLED", "1")

	cfg := &testConf{Optional: &optional{}}
	if err := OverrideFromEnv(cfg); err != nil {
		t.Fatalf("OverrideFromEnv: %v", err)
	}

	if cfg.Name != "n" {
		t.Errorf("Name = %q", cfg.Name)
	}
	if cfg.Timeout != 90*time.Second {
		t.Errorf("Timeout = %v, want 1m30s", cfg.Timeout)
	}
	if cfg.Ratio != 0.25 {
		t.Errorf("Ratio = %v", cfg.Ratio)
	}
	if cfg.Count != 65535 {
		t.Errorf("Count = %d", cfg.Count)
	}
	if !cfg.Debug {
		t.Error("Debug = false, want true")
	}
	if !cfg.Optional.Enabled {
		t.Error("Optional.Enabled = false, want true (非 nil 结构体指针应被递归)")
	}
}

// nil 结构体指针不递归，也不能 panic
func TestOverrideFromEnvNilPointerSkipped(t *testing.T) {
	t.Setenv("TEST_OPT_ENABLED", "true")

	cfg := &testConf{}
	if err := OverrideFromEnv(cfg); err != nil {
		t.Fatalf("OverrideFromEnv: %v", err)
	}
	if cfg.Optional != nil {
		t.Errorf("Optional = %+v, want nil", cfg.Optional)
	}
}

// 环境变量为空串等于没设
func TestOverrideFromEnvEmptyValueSkipped(t *testing.T) {
	t.Setenv("TEST_NAME", "")

	cfg := &testConf{Name: "keep"}
	if err := OverrideFromEnv(cfg); err != nil {
		t.Fatalf("OverrideFromEnv: %v", err)
	}
	if cfg.Name != "keep" {
		t.Errorf("Name = %q, want keep", cfg.Name)
	}
}

// 解析失败必须返回 error，不能静默变成零值
func TestOverrideFromEnvParseErrors(t *testing.T) {
	tests := []struct {
		name string
		env  string
		val  string
	}{
		{"bad int", "TEST_DB_PORT", "not-a-number"},
		{"bad duration", "TEST_TIMEOUT", "30"},
		{"bad float", "TEST_RATIO", "abc"},
		{"bad bool", "TEST_DEBUG", "yes-please"},
		{"uint overflow", "TEST_COUNT", "70000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(tt.env, tt.val)
			err := OverrideFromEnv(&testConf{})
			if err == nil {
				t.Fatalf("%s=%q should return an error", tt.env, tt.val)
			}
			if !strings.Contains(err.Error(), tt.env) {
				t.Errorf("error should name the env var, got: %v", err)
			}
		})
	}
}

// 多个错误一次报全
func TestOverrideFromEnvJoinsErrors(t *testing.T) {
	t.Setenv("TEST_DB_PORT", "bad")
	t.Setenv("TEST_RATIO", "bad")

	err := OverrideFromEnv(&testConf{})
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"TEST_DB_PORT", "TEST_RATIO"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %s, got: %v", want, err)
		}
	}
}

func TestOverrideFromEnvBadTarget(t *testing.T) {
	tests := []struct {
		name string
		out  any
	}{
		{"not a pointer", testConf{}},
		{"nil pointer", (*testConf)(nil)},
		{"pointer to non-struct", new(int)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := OverrideFromEnv(tt.out); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestLoadIntoMissingFile(t *testing.T) {
	if err := LoadInto(filepath.Join(t.TempDir(), "nope.yaml"), &testConf{}); err == nil {
		t.Fatal("want error for missing file")
	}
}

func TestLoadIntoBadYaml(t *testing.T) {
	path := writeConfig(t, "name: [unclosed\n")
	if err := LoadInto(path, &testConf{}); err == nil {
		t.Fatal("want error for malformed yaml")
	}
}

// 未导出字段不能让反射 panic
func TestOverrideFromEnvIgnoresUnexported(t *testing.T) {
	t.Setenv("TEST_NAME", "ok")
	cfg := &testConf{hidden: "x"}
	if err := OverrideFromEnv(cfg); err != nil {
		t.Fatalf("OverrideFromEnv: %v", err)
	}
	if cfg.hidden != "x" {
		t.Errorf("hidden = %q, want x", cfg.hidden)
	}
}
