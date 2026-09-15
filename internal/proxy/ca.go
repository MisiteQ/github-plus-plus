package proxy

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// CA 是本地的证书颁发机构，用于对 GitHub 域名做 TLS 中间人解密。
//
// 工作方式：为每个被加速的域名动态签发证书，客户端只需信任这张根证书，
// 之后所有到 GitHub 的 HTTPS 流量都能被本服务解密、改写并加速。
//
// 安全考量：
//   - 私钥仅保存在本机数据目录，权限 0600；
//   - 证书只对 GitHub 相关域名签发，不会成为通用中间人；
//   - 服务停止后不再解密任何流量。
type CA struct {
	cert    *x509.Certificate
	certPEM []byte
	keyPEM  []byte
	priv    *rsa.PrivateKey

	// cache 缓存已签发的域名证书，避免每个连接都重新生成。
	mu    sync.RWMutex
	cache map[string]*tls.Certificate
}

// LoadOrCreateCA 从目录加载 CA，不存在则生成一套新的。
func LoadOrCreateCA(dir string) (*CA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("创建证书目录失败: %w", err)
	}

	certPath := filepath.Join(dir, "ghpp-ca.crt")
	keyPath := filepath.Join(dir, "ghpp-ca.key")

	// 已存在则直接加载。
	if _, err := os.Stat(certPath); err == nil {
		if _, err := os.Stat(keyPath); err == nil {
			ca, err := loadCA(certPath, keyPath)
			if err == nil {
				return ca, nil
			}
			// 加载失败（例如文件损坏）时重新生成，不阻断启动。
		}
	}

	return generateCA(certPath, keyPath)
}

// CAFilePaths 返回 CA 证书与私钥的存放路径。
func CAFilePaths(dir string) (certPath, keyPath string) {
	return filepath.Join(dir, "ghpp-ca.crt"), filepath.Join(dir, "ghpp-ca.key")
}

// ImportCA 用用户提供的 PEM 证书与私钥替换本地 CA。
//
// 用于让多台设备共用同一套根证书，或导入用户自己的证书体系。
// 导入前会校验证书与私钥匹配，并把旧文件备份为 *.bak-<时间戳>，
// 保证出错时可以手工恢复。
func ImportCA(dir string, certPEM, keyPEM []byte) (*CA, error) {
	// 先校验合法性，通过后才落盘。
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("证书与私钥不匹配或格式无效: %w", err)
	}
	cert, err := x509.ParseCertificate(tlsCert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("解析证书失败: %w", err)
	}
	if !cert.IsCA {
		return nil, fmt.Errorf("导入的证书必须是 CA 证书（含 CA:TRUE 基本约束）")
	}
	if _, ok := tlsCert.PrivateKey.(*rsa.PrivateKey); !ok {
		return nil, fmt.Errorf("私钥类型不受支持，请使用 RSA 私钥")
	}
	if cert.NotAfter.Before(time.Now()) {
		return nil, fmt.Errorf("证书已过期（%s）", cert.NotAfter.Format("2006-01-02"))
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("创建证书目录失败: %w", err)
	}
	certPath, keyPath := CAFilePaths(dir)

	// 备份现有文件，失败不阻断导入。
	backup := func(p string) {
		data, err := os.ReadFile(p)
		if err != nil {
			return
		}
		_ = os.WriteFile(fmt.Sprintf("%s.bak-%d", p, time.Now().Unix()), data, 0o600)
	}
	backup(certPath)
	backup(keyPath)

	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, fmt.Errorf("写入证书失败: %w", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("写入私钥失败: %w", err)
	}

	return &CA{
		cert:    cert,
		certPEM: certPEM,
		keyPEM:  keyPEM,
		priv:    tlsCert.PrivateKey.(*rsa.PrivateKey),
		cache:   make(map[string]*tls.Certificate),
	}, nil
}

// RegenerateCA 生成一套全新的根证书，替换现有证书。
// 旧证书会被备份，安装过旧证书的客户端需要重新安装。
func RegenerateCA(dir string) (*CA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("创建证书目录失败: %w", err)
	}
	certPath, keyPath := CAFilePaths(dir)

	backup := func(p string) {
		data, err := os.ReadFile(p)
		if err != nil {
			return
		}
		_ = os.WriteFile(fmt.Sprintf("%s.bak-%d", p, time.Now().Unix()), data, 0o600)
	}
	backup(certPath)
	backup(keyPath)

	return generateCA(certPath, keyPath)
}

// loadCA 从磁盘读取已有的根证书与私钥。
func loadCA(certPath, keyPath string) (*CA, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("证书与私钥不匹配: %w", err)
	}
	cert, err := x509.ParseCertificate(tlsCert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("解析证书失败: %w", err)
	}

	priv, ok := tlsCert.PrivateKey.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("私钥类型不受支持")
	}

	return &CA{
		cert:    cert,
		certPEM: certPEM,
		keyPEM:  keyPEM,
		priv:    priv,
		cache:   make(map[string]*tls.Certificate),
	}, nil
}

// generateCA 生成一套全新的根证书。
func generateCA(certPath, keyPath string) (*CA, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("生成私钥失败: %w", err)
	}

	serial, err := randSerial()
	if err != nil {
		return nil, err
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "GitHub++ 本地加速根证书",
			Organization: []string{"GitHub++"},
			Country:      []string{"CN"},
		},
		NotBefore:             time.Now().Add(-24 * time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, fmt.Errorf("签发根证书失败: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(priv),
	})

	// 证书公开无妨，私钥必须严格限制权限。
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, fmt.Errorf("写入根证书失败: %w", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("写入私钥失败: %w", err)
	}

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}

	return &CA{
		cert:    cert,
		certPEM: certPEM,
		keyPEM:  keyPEM,
		priv:    priv,
		cache:   make(map[string]*tls.Certificate),
	}, nil
}

// CertificateFor 返回某个域名的证书，必要时即时签发并缓存。
func (ca *CA) CertificateFor(host string) (*tls.Certificate, error) {
	ca.mu.RLock()
	if c, ok := ca.cache[host]; ok {
		ca.mu.RUnlock()
		return c, nil
	}
	ca.mu.RUnlock()

	cert, err := ca.issue(host)
	if err != nil {
		return nil, err
	}

	ca.mu.Lock()
	ca.cache[host] = cert
	ca.mu.Unlock()
	return cert, nil
}

// issue 为指定域名签发一张叶证书。
func (ca *CA) issue(host string) (*tls.Certificate, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("生成域名私钥失败: %w", err)
	}

	serial, err := randSerial()
	if err != nil {
		return nil, err
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   host,
			Organization: []string{"GitHub++"},
		},
		NotBefore:             time.Now().Add(-24 * time.Hour),
		NotAfter:              time.Now().AddDate(2, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{host},
	}

	// 通配符域名同时列出子域，提升兼容性。
	if strings.HasPrefix(host, "*.") {
		tmpl.DNSNames = []string{host, strings.TrimPrefix(host, "*.")}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &priv.PublicKey, ca.priv)
	if err != nil {
		return nil, fmt.Errorf("签发 %s 的证书失败: %w", host, err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(priv),
	})

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	return &tlsCert, nil
}

// CertPEM 返回根证书的 PEM 内容，供用户下载安装。
func (ca *CA) CertPEM() []byte { return ca.certPEM }

// Fingerprint 返回根证书的 SHA256 指纹，便于用户核验。
func (ca *CA) Fingerprint() string {
	sum := sha256Sum(ca.cert.Raw)
	parts := make([]string, 0, len(sum))
	for _, b := range sum {
		parts = append(parts, fmt.Sprintf("%02X", b))
	}
	return strings.Join(parts, ":")
}

// NotAfter 返回根证书的过期时间。
func (ca *CA) NotAfter() time.Time { return ca.cert.NotAfter }

// randSerial 生成一个随机的证书序列号。
func randSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("生成序列号失败: %w", err)
	}
	return n, nil
}
