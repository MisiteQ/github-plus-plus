package dnsopt

import (
	"encoding/json"
	"fmt"
	"io"
)

// decodeJSON 读取并解析 JSON，集中处理错误信息以便调试。
func decodeJSON(r io.Reader, v any) error {
	data, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return fmt.Errorf("读取响应失败: %w", err)
	}
	if len(data) == 0 {
		return fmt.Errorf("响应为空")
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("解析 JSON 失败: %w", err)
	}
	return nil
}
