//go:build !windows

package config

// isWindows 报告当前是否运行在 Windows 平台。
func isWindows() bool { return false }
