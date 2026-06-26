// package fetcher は HTTP/HTTPS でコンテンツを取得する（net/http）。
// DES-001 §7.2: タイムアウト 30s・リダイレクト最大 5 回・SSRF 対策・Content-Type チェック。
package fetcher

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Fetcher は URL からコンテンツを取得するインターフェース。
// テスト時にモック実装で差し替え可能にするためにインターフェースとして定義する。
type Fetcher interface {
	// Fetch は url のコンテンツを取得して返す。
	// Content-Type が text/ 系以外の場合はエラーを返す。
	// SSRF 対策: プライベート IP アドレスへのリクエストはブロックする（DES-001 §7.2）。
	Fetch(ctx context.Context, url string) (string, error)
}

// Config は Fetcher の設定。設定値は config.FetcherConfig から組み立てる（DES-001 §9.1）。
type Config struct {
	// TimeoutSecs はフェッチタイムアウト秒数（doc-db.yaml: fetcher.timeout_seconds）。
	TimeoutSecs int

	// AllowPrivate が true の場合、プライベート IP へのリクエストを許可する（SSRF 対策を無効化）。
	// （doc-db.yaml: fetcher.allow_private）
	AllowPrivate bool
}

// httpFetcher は net/http を使った Fetcher 実装。
type httpFetcher struct {
	cfg    Config
	client *http.Client
}

// New は Config を使って Fetcher を生成する。
func New(cfg Config) Fetcher {
	if cfg.TimeoutSecs <= 0 {
		cfg.TimeoutSecs = 30
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// SSRF 対策: DNS 解決後の IP アドレスを検証する（DES-001 §7.2）
			if !cfg.AllowPrivate {
				host, _, err := net.SplitHostPort(addr)
				if err != nil {
					host = addr
				}
				ips, err := net.DefaultResolver.LookupHost(ctx, host)
				if err != nil {
					return nil, fmt.Errorf("fetcher: DNS 解決失敗 %q: %w", host, err)
				}
				for _, ipStr := range ips {
					ip := net.ParseIP(ipStr)
					if ip == nil {
						continue
					}
					if isPrivateIP(ip) {
						return nil, fmt.Errorf("fetcher: SSRF ブロック — プライベート IP アドレスへのリクエストは許可されていません: %s → %s", addr, ipStr)
					}
				}
			}
			d := &net.Dialer{}
			return d.DialContext(ctx, network, addr)
		},
	}

	client := &http.Client{
		Timeout:   time.Duration(cfg.TimeoutSecs) * time.Second,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("fetcher: リダイレクト上限（5回）を超えました")
			}
			return nil
		},
	}

	return &httpFetcher{cfg: cfg, client: client}
}

// Fetch は url のコンテンツを取得して文字列で返す。
func (f *httpFetcher) Fetch(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("fetcher: リクエスト作成失敗 %q: %w", url, err)
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetcher: HTTP リクエスト失敗 %q: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("fetcher: HTTP %d %s (%s)", resp.StatusCode, resp.Status, url)
	}

	// Content-Type チェック: text/ 系以外はスキップ（DES-001 §7.2）
	ct := resp.Header.Get("Content-Type")
	if ct != "" && !strings.HasPrefix(strings.ToLower(ct), "text/") {
		return "", fmt.Errorf("fetcher: Content-Type %q は text/ 系ではないためスキップします (%s)", ct, url)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("fetcher: レスポンス読み込み失敗 %q: %w", url, err)
	}

	return string(body), nil
}

// isPrivateIP は IP アドレスがプライベートまたはループバックか判定する（DES-001 §7.2 SSRF 対策）。
// ブロック対象:
//   - 127.0.0.0/8  (ループバック)
//   - 10.0.0.0/8   (RFC1918 プライベート)
//   - 172.16.0.0/12 (RFC1918 プライベート)
//   - 192.168.0.0/16 (RFC1918 プライベート)
//   - 169.254.0.0/16 (リンクローカル / AWS IMDS 等)
//   - ::1           (IPv6 ループバック)
//   - fc00::/7      (IPv6 ユニークローカル)
func isPrivateIP(ip net.IP) bool {
	privateRanges := []string{
		"127.0.0.0/8",
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"169.254.0.0/16",
		"::1/128",
		"fc00::/7",
	}
	for _, cidr := range privateRanges {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		if network.Contains(ip) {
			return true
		}
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()
}
