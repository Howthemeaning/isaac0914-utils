// Package config 从 yaml 文件加载配置，再用环境变量覆盖。
//
// 覆盖顺序是 结构体默认值 → yaml → 环境变量。默认值写在 Go 代码里，
// 配置文件缺项时行为确定，不会悄悄拿到零值：
//
//	cfg := &Config{Addr: ":8080"}
//	if err := config.LoadInto("config.yaml", cfg); err != nil {
//	    return err
//	}
//
// 环境变量用 env tag 声明，优先级最高：
//
//	type DB struct {
//	    Host string `yaml:"host" env:"DB_HOST"`
//	}
//
// 限制：只递归结构体和非 nil 结构体指针。slice、map 元素里的 env tag 不生效——
// slice 的每个元素会拿到同一个环境变量值，语义上说不通。环境变量为空串等于没设，
// 无法用环境变量把字段显式置成空串。
package config

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

// envTag 声明字段对应的环境变量名
const envTag = "env"

var durationType = reflect.TypeOf(time.Duration(0))

// LoadInto 读 path 处的 yaml 填充 out，再用环境变量覆盖带 env tag 的字段。
// out 必须是非 nil 结构体指针，可以预先带好默认值。
func LoadInto(path string, out any) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("config: read %s: %w", path, err)
	}
	if err := yaml.Unmarshal(content, out); err != nil {
		return fmt.Errorf("config: parse %s: %w", path, err)
	}
	return OverrideFromEnv(out)
}

// OverrideFromEnv 只做环境变量覆盖，供没有配置文件的场景单独使用。
// 所有解析失败汇总成一个 error 返回，不静默忽略——配错的值不该悄悄变成零值。
func OverrideFromEnv(out any) error {
	v := reflect.ValueOf(out)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return fmt.Errorf("config: out must be a non-nil pointer, got %T", out)
	}
	if v.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("config: out must point to a struct, got pointer to %s", v.Elem().Kind())
	}
	var errs []error
	walk(v.Elem(), &errs)
	return errors.Join(errs...)
}

// walk 遍历结构体字段，对带 env tag 的字段做覆盖，遇到嵌套结构体继续下探
func walk(v reflect.Value, errs *[]error) {
	t := v.Type()
	for i := range t.NumField() {
		field, value := t.Field(i), v.Field(i)
		if !field.IsExported() {
			continue
		}

		switch {
		case value.Kind() == reflect.Struct && value.Type() != durationType:
			walk(value, errs)
			continue
		case value.Kind() == reflect.Pointer && value.Type().Elem().Kind() == reflect.Struct:
			if !value.IsNil() {
				walk(value.Elem(), errs)
			}
			continue
		}

		name := field.Tag.Get(envTag)
		if name == "" {
			continue
		}
		raw := os.Getenv(name)
		if raw == "" {
			continue
		}
		if !value.CanSet() {
			*errs = append(*errs, fmt.Errorf("config: field %s.%s is not settable", t.Name(), field.Name))
			continue
		}
		if err := setValue(value, raw); err != nil {
			*errs = append(*errs, fmt.Errorf("config: env %s=%q: %w", name, raw, err))
		}
	}
}

// setValue 把环境变量的字符串值按字段类型解析后写入
func setValue(v reflect.Value, raw string) error {
	if v.Type() == durationType {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return err
		}
		v.SetInt(int64(d))
		return nil
	}

	switch v.Kind() {
	case reflect.String:
		v.SetString(raw)
	case reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return err
		}
		v.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(raw, 10, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetUint(n)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(raw, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetFloat(f)
	default:
		return fmt.Errorf("unsupported field type %s", v.Type())
	}
	return nil
}
