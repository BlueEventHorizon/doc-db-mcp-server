package fetcher

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// -----------------------------------------------------------------------
// isPrivateIP の単体検証
// -----------------------------------------------------------------------

func TestIsPrivateIP_BlocksAllListedRanges(t *testing.T) {
	private := []string{
		"127.0.0.1",   // ループバック
		"127.5.5.5",   // /8 全域
		"10.0.0.1",    // RFC1918
		"10.255.255.255",
		"172.16.0.1",  // RFC1918
		"172.31.255.255",
		"192.168.0.1", // RFC1918
		"192.168.5.5",
		"169.254.0.1", // リンクローカル / AWS IMDS
		"::1",         // IPv6 ループバック
		"fc00::1",     // IPv6 ユニークローカル
		"fd12::5",
	}
	for _, s := range private {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("ParseIP(%q) failed", s)
		}
		if !isPrivateIP(ip) {
			t.Errorf("isPrivateIP(%s) = false, want true (must be blocked)", s)
		}
	}
}

func TestIsPrivateIP_PassesPublicIPs(t *testing.T) {
	public := []string{
		"8.8.8.8",
		"1.1.1.1",
		"172.15.255.255", // 172.16/12 のすぐ外
		"172.32.0.1",     // 172.16/12 のすぐ外
		"169.253.255.255",
		"169.255.0.1",
		"2001:db8::1",
	}
	for _, s := range public {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("ParseIP(%q) failed", s)
		}
		if isPrivateIP(ip) {
			t.Errorf("isPrivateIP(%s) = true, want false (should pass)", s)
		}
	}
}

// -----------------------------------------------------------------------
// SSRF 統合: AllowPrivate=false で httptest 127.0.0.1 がブロックされる
// -----------------------------------------------------------------------

func TestFetch_BlocksPrivateByDefault(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok"))
	}))
	defer ts.Close()

	f := New(Config{TimeoutSecs: 5, AllowPrivate: false})
	_, err := f.Fetch(context.Background(), ts.URL)
	if err == nil {
		t.Fatal("want SSRF block error, got nil")
	}
	if !strings.Contains(err.Error(), "SSRF") {
		t.Errorf("err = %v, want SSRF block message", err)
	}
}

func TestFetch_AllowPrivateLetsThrough(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("hello"))
	}))
	defer ts.Close()

	f := New(Config{TimeoutSecs: 5, AllowPrivate: true})
	got, err := f.Fetch(context.Background(), ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello" {
		t.Errorf("body = %q, want hello", got)
	}
}

// -----------------------------------------------------------------------
// Content-Type チェック
// -----------------------------------------------------------------------

func TestFetch_RejectsNonTextContentType(t *testing.T) {
	cases := []string{
		"application/octet-stream",
		"image/png",
		"application/json",
	}
	for _, ct := range cases {
		t.Run(ct, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", ct)
				_, _ = w.Write([]byte("binary"))
			}))
			defer ts.Close()

			f := New(Config{TimeoutSecs: 5, AllowPrivate: true})
			_, err := f.Fetch(context.Background(), ts.URL)
			if err == nil {
				t.Fatalf("Content-Type %s: want error", ct)
			}
			if !strings.Contains(err.Error(), "Content-Type") {
				t.Errorf("err = %v, want Content-Type-related message", err)
			}
		})
	}
}

func TestFetch_AcceptsTextTypes(t *testing.T) {
	cases := []string{
		"text/plain",
		"text/html; charset=utf-8",
		"text/markdown",
	}
	for _, ct := range cases {
		t.Run(ct, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", ct)
				_, _ = w.Write([]byte("ok"))
			}))
			defer ts.Close()

			f := New(Config{TimeoutSecs: 5, AllowPrivate: true})
			got, err := f.Fetch(context.Background(), ts.URL)
			if err != nil {
				t.Errorf("Content-Type %s: unexpected err %v", ct, err)
			}
			if got != "ok" {
				t.Errorf("body = %q, want ok", got)
			}
		})
	}
}

// -----------------------------------------------------------------------
// HTTP ステータスコード
// -----------------------------------------------------------------------

func TestFetch_NonSuccessStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer ts.Close()

	f := New(Config{TimeoutSecs: 5, AllowPrivate: true})
	_, err := f.Fetch(context.Background(), ts.URL)
	if err == nil {
		t.Fatal("want error on 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v, want '500' in message", err)
	}
}

// -----------------------------------------------------------------------
// リダイレクト上限
// -----------------------------------------------------------------------

func TestFetch_RedirectLimit(t *testing.T) {
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 常にリダイレクトを返す（無限ループ）
		http.Redirect(w, r, ts.URL+"/next", http.StatusFound)
	}))
	defer ts.Close()

	f := New(Config{TimeoutSecs: 5, AllowPrivate: true})
	_, err := f.Fetch(context.Background(), ts.URL)
	if err == nil {
		t.Fatal("want redirect-limit error")
	}
	// Go の http.Client はリダイレクト超過時に "stopped after ..." を含めるか
	// CheckRedirect のメッセージを表に出す（Go バージョン依存だが上限超過した事実は err に含まれる）
	if !strings.Contains(err.Error(), "リダイレクト") && !strings.Contains(err.Error(), "stopped after") {
		t.Errorf("err = %v, want redirect-related message", err)
	}
}

func TestFetch_FollowsRedirectsBelowLimit(t *testing.T) {
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /final で成功、それ以外は次のホップへ
		switch r.URL.Path {
		case "/final":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("end"))
		default:
			// 2 hops 後に /final へ
			http.Redirect(w, r, ts.URL+nextPath(r.URL.Path), http.StatusFound)
		}
	}))
	defer ts.Close()

	f := New(Config{TimeoutSecs: 5, AllowPrivate: true})
	got, err := f.Fetch(context.Background(), ts.URL+"/a")
	if err != nil {
		t.Fatal(err)
	}
	if got != "end" {
		t.Errorf("body = %q, want end", got)
	}
}

// nextPath はテスト用の小さな状態機械: /a → /b → /final。
func nextPath(p string) string {
	switch p {
	case "/a":
		return "/b"
	default:
		return "/final"
	}
}

// -----------------------------------------------------------------------
// デフォルト値
// -----------------------------------------------------------------------

func TestNew_DefaultTimeout(t *testing.T) {
	cases := []int{0, -5}
	for _, ts := range cases {
		t.Run(fmt.Sprintf("in=%d", ts), func(t *testing.T) {
			f := New(Config{TimeoutSecs: ts, AllowPrivate: true}).(*httpFetcher)
			if f.client.Timeout.Seconds() != 30 {
				t.Errorf("timeout = %v, want 30s default", f.client.Timeout)
			}
		})
	}
}
