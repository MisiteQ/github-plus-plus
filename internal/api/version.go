package api

import "encoding/json"

// Version 是当前程序版本号，由构建脚本通过 -ldflags 注入。
var Version = "dev"

// marshalJSON 序列化 JSON 且不转义 HTML 字符。
func marshalJSON(v any) ([]byte, error) {
	buf, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return buf, nil
}
