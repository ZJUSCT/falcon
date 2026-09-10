package config

import (
	"fmt"
	"os"
	"reflect"
	"strings"
)

// 只替换解码后的字符串值，避免环境变量中的引号、换行等被解释为 YAML 结构。
// 保留字段类型和 map 键，不将数字、布尔值或列表解析成另一种配置结构。
func expandConfigEnv(value reflect.Value, path string) error {
	switch value.Kind() {
	case reflect.String:
		expanded, err := expandEnvString(value.String())
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		value.SetString(expanded)
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			name := strings.Split(value.Type().Field(i).Tag.Get("json"), ",")[0]
			if path != "" {
				name = path + "." + name
			}
			if err := expandConfigEnv(value.Field(i), name); err != nil {
				return err
			}
		}
	case reflect.Slice:
		for i := 0; i < value.Len(); i++ {
			if err := expandConfigEnv(value.Index(i), path+"[]"); err != nil {
				return err
			}
		}
	case reflect.Map:
		iter := value.MapRange()
		for iter.Next() {
			// map 元素不可直接修改，先复制，替换后写回原键。
			item := reflect.New(iter.Value().Type()).Elem()
			item.Set(iter.Value())
			if err := expandConfigEnv(item, path+"[]"); err != nil {
				return err
			}
			value.SetMapIndex(iter.Key(), item)
		}
	}
	return nil
}

// ${NAME} 引用非空环境变量，$${...} 保留字面量 ${...}。
// 不处理 $NAME，也不再次扫描替换得到的值。错误仅包含变量名，不包含配置值。
func expandEnvString(input string) (string, error) {
	var result strings.Builder
	for i := 0; i < len(input); {
		escaped := strings.HasPrefix(input[i:], "$${")
		if !escaped && !strings.HasPrefix(input[i:], "${") {
			result.WriteByte(input[i])
			i++
			continue
		}
		start := i + 2
		if escaped {
			start++
		}
		end := strings.IndexByte(input[start:], '}')
		if end < 0 {
			return "", fmt.Errorf("unterminated environment placeholder")
		}
		end += start
		if escaped {
			result.WriteString(input[i+1 : end+1])
		} else {
			name := input[start:end]
			if !validEnvName(name) {
				return "", fmt.Errorf("environment placeholder must use ${NAME}, where NAME matches [A-Za-z_][A-Za-z0-9_]*")
			}
			value, ok := os.LookupEnv(name)
			if !ok || value == "" {
				return "", fmt.Errorf("environment variable %q must be set and non-empty", name)
			}
			result.WriteString(value)
		}
		i = end + 1
	}
	return result.String(), nil
}

func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, c := range name {
		if c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || i > 0 && c >= '0' && c <= '9' {
			continue
		}
		return false
	}
	return true
}
