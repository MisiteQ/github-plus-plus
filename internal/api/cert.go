package api

import (
	"fmt"
	"net/http"
	"strings"
)

// handleCert 提供根证书相关的接口。
//
// 支持的操作用 path 与方法区分：
//
//	GET   /api/cert             查看证书状态（指纹、有效期、安装指引）
//	GET   /api/cert/download    下载根证书文件
//	GET   /api/cert/script      获取一键安装脚本
//	POST  /api/cert/import      导入用户自己的 CA（PEM 证书 + 私钥）
//	POST  /api/cert/regenerate  重新生成一套全新的本地 CA
func (s *Server) handleCert(w http.ResponseWriter, r *http.Request) {
	ca := s.app.CA()

	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/import"):
		var body struct {
			CertPEM string `json:"cert_pem"`
			KeyPEM  string `json:"key_pem"`
		}
		if err := decodeBody(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, "%v", err)
			return
		}
		if body.CertPEM == "" || body.KeyPEM == "" {
			writeError(w, http.StatusBadRequest, "需要同时提供 cert_pem 与 key_pem")
			return
		}
		if err := s.app.ReplaceCA("import", []byte(body.CertPEM), []byte(body.KeyPEM)); err != nil {
			writeError(w, http.StatusBadRequest, "%v", err)
			return
		}
		newCA := s.app.CA()
		writeOK(w, map[string]any{
			"replaced":    true,
			"fingerprint": newCA.Fingerprint(),
			"not_after":   newCA.NotAfter(),
			"note":        "新证书已热生效。之前安装过旧证书的设备需要重新安装并信任新证书。",
		})
		return

	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/regenerate"):
		if err := s.app.ReplaceCA("regenerate", nil, nil); err != nil {
			writeError(w, http.StatusInternalServerError, "%v", err)
			return
		}
		newCA := s.app.CA()
		writeOK(w, map[string]any{
			"replaced":    true,
			"fingerprint": newCA.Fingerprint(),
			"not_after":   newCA.NotAfter(),
			"note":        "已生成全新根证书并热生效。所有客户端需重新下载安装。",
		})
		return

	case strings.HasSuffix(r.URL.Path, "/download"):
		if ca == nil {
			writeError(w, http.StatusServiceUnavailable, "根证书不可用: %v", s.app.CAError())
			return
		}
		w.Header().Set("Content-Type", "application/x-x509-ca-cert")
		w.Header().Set("Content-Disposition", `attachment; filename="ghpp-ca.crt"`)
		_, _ = w.Write(ca.CertPEM())
		return

	case strings.HasSuffix(r.URL.Path, "/script"):
		if ca == nil {
			writeError(w, http.StatusServiceUnavailable, "根证书不可用: %v", s.app.CAError())
			return
		}
		platform := r.URL.Query().Get("platform")
		script := installScript(platform, s.caDownloadURL(r))
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(script))
		return
	}

	// 默认返回证书状态。
	if ca == nil {
		writeOK(w, map[string]any{
			"available": false,
			"error":     fmt.Sprintf("%v", s.app.CAError()),
		})
		return
	}

	cfg := s.app.CurrentConfig()
	writeOK(w, map[string]any{
		"available":   true,
		"fingerprint": ca.Fingerprint(),
		"not_after":   ca.NotAfter(),
		"subject":     "GitHub++ 本地加速根证书",
		"download":    "/api/cert/download",
		"mitm_active": true,
		"note": "安装此证书后，浏览器访问 GitHub 网页也将走加速通道。" +
			"证书仅对 GitHub 相关域名签发，私钥保存在本机 " + cfg.DataDir + " 目录下。",
	})
}

// caDownloadURL 构造供客户端下载根证书的地址。
//
// 优先使用请求中的 Host，这样用户在局域网里访问控制台时
// 拿到的脚本能直接指向 NAS 的真实地址。
func (s *Server) caDownloadURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}
	host := r.Host
	if host == "" {
		host = "127.0.0.1:7717"
	}
	return fmt.Sprintf("%s://%s/api/cert/download", scheme, host)
}

// installScript 生成各平台的根证书安装脚本。
//
// 脚本都包含清晰的回滚说明，避免用户装完不知道怎么卸载。
func installScript(platform, downloadURL string) string {
	switch platform {
	case "macos":
		return `#!/bin/bash
# GitHub++ 根证书安装脚本（macOS）
# 需要管理员权限，执行后会要求输入密码。
set -e

CERT_FILE="/tmp/ghpp-ca.crt"

echo "正在下载根证书..."
curl -fsSL "` + downloadURL + `" -o "$CERT_FILE"

echo "正在安装到系统钥匙串（需要输入密码）..."
sudo security add-trusted-cert -d -r trustRoot \
  -k /Library/Keychains/System.keychain "$CERT_FILE"

echo ""
echo "安装完成。"
echo "如需卸载，执行："
echo "  1. 打开「钥匙串访问」"
echo "  2. 搜索「GitHub++ 本地加速根证书」"
echo "  3. 删除该证书"
`
	case "linux":
		return `#!/bin/bash
# GitHub++ 根证书安装脚本（Linux）
# 支持 Debian/Ubuntu 与 RHEL/CentOS 系发行版。
set -e

CERT_FILE="/tmp/ghpp-ca.crt"

echo "正在下载根证书..."
curl -fsSL "` + downloadURL + `" -o "$CERT_FILE"

if command -v update-ca-certificates >/dev/null 2>&1; then
  sudo cp "$CERT_FILE" /usr/local/share/ca-certificates/ghpp-ca.crt
  sudo update-ca-certificates
  echo "已安装到系统信任库。"
elif command -v update-ca-trust >/dev/null 2>&1; then
  sudo cp "$CERT_FILE" /etc/pki/ca-trust/source/anchors/ghpp-ca.crt
  sudo update-ca-trust
  echo "已安装到系统信任库。"
else
  echo "未识别的发行版，请手动把证书导入系统信任库。"
  exit 1
fi

# Firefox 与 Chrome 使用独立信任库，需要额外处理。
if [ -d "$HOME/.pki/nssdb" ]; then
  if command -v certutil >/dev/null 2>&1; then
    certutil -d sql:"$HOME/.pki/nssdb" -A -t "C,," -n "GitHub++" -i "$CERT_FILE" || true
    echo "已安装到 Chrome/Chromium 信任库。"
  fi
fi

echo ""
echo "安装完成。"
echo "如需卸载："
echo "  sudo rm /usr/local/share/ca-certificates/ghpp-ca.crt && sudo update-ca-certificates --fresh"
`
	default: // windows
		// PowerShell 的转义符是反引号，与 Go 的反引号原始字符串冲突，
		// 因此这里用普通字符串拼接，避免转义地狱。
		return "# GitHub++ 根证书安装脚本（Windows PowerShell）\n" +
			"# 请以管理员身份运行 PowerShell 后执行本脚本。\n\n" +
			"$ErrorActionPreference = \"Stop\"\n" +
			"$DownloadUrl = \"" + downloadURL + "\"\n" +
			"$CertFile = \"$env:TEMP\\ghpp-ca.crt\"\n\n" +
			"Write-Host \"正在下载根证书...\" -ForegroundColor Cyan\n" +
			"Invoke-WebRequest -Uri $DownloadUrl -OutFile $CertFile -UseBasicParsing\n\n" +
			"Write-Host \"正在导入到「受信任的根证书颁发机构」...\" -ForegroundColor Cyan\n" +
			"$cert = Import-Certificate -FilePath $CertFile -CertStoreLocation Cert:\\LocalMachine\\Root\n\n" +
			"Write-Host \"\"\n" +
			"Write-Host \"安装完成，证书指纹：\" -ForegroundColor Green\n" +
			"Write-Host \"  $($cert.Thumbprint)\"\n" +
			"Write-Host \"\"\n" +
			"Write-Host \"如需卸载，请在管理员 PowerShell 中执行：\"\n" +
			"Write-Host \"  Get-ChildItem Cert:\\LocalMachine\\Root | Where-Object { $_.Subject -like '*GitHub++*' } | Remove-Item\"\n"
	}
}
