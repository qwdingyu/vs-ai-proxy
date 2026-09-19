package proxy

import (
	"net/http"
	"testing"

	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

// ---------------------------------------------------------------------------
// Anthropic 直连路径的上游 HTTP 客户端纪律
//
// 背景：本包 5 个直连上游的调用点（非流式转换、流式转换、管理端流式兜底、
// 两种 passthrough）原先共用 http.DefaultClient。它会继承 http.DefaultTransport，
// 带来两个已实测确认的差异：
//
//  1. DisableCompression=false → 传输层声明 Accept-Encoding: gzip 并透明解压，
//     在流式响应前插入解压缓冲，破坏"边收边吐"（已用 httptest 实测：
//     DefaultClient 发出 Accept-Encoding: gzip，标准 client 不发出）。
//  2. MaxIdleConnsPerHost=2 → 并发场景频繁重建 TCP+TLS。
//
// 这些测试锁定"不得退回 DefaultClient"，因为退回后功能仍能跑通，
// 只有流式流畅性与并发表现会退化，属于最难被发现的回归。
// ---------------------------------------------------------------------------

// anthropicUpstreamClient 必须与 provider 转发路径使用同一套传输纪律。
func TestAnthropicUpstreamClientUsesDisciplinedTransport(t *testing.T) {
	if anthropicUpstreamClient == nil {
		t.Fatal("anthropicUpstreamClient 未初始化")
	}
	if anthropicUpstreamClient == http.DefaultClient {
		t.Fatal("anthropicUpstreamClient 是 http.DefaultClient，会继承 DefaultTransport 的不良默认值")
	}

	transport, ok := anthropicUpstreamClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport 类型 = %T, want *http.Transport", anthropicUpstreamClient.Transport)
	}

	if !transport.DisableCompression {
		t.Error("DisableCompression = false，流式响应会被透明 gzip 缓冲，破坏增量输出")
	}
	if transport.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 = true，want false")
	}
	if transport.MaxIdleConnsPerHost <= http.DefaultMaxIdleConnsPerHost {
		t.Errorf("MaxIdleConnsPerHost = %d，未超过默认值 %d，并发下会频繁重建 TLS",
			transport.MaxIdleConnsPerHost, http.DefaultMaxIdleConnsPerHost)
	}
	if transport.Proxy == nil {
		t.Error("Proxy = nil，云主机/企业代理环境会失效")
	}
}

// 流式路径必须不设客户端级超时：client.Timeout 会连同流式 body 一起计时，
// 设置它会让长回复被提前截断。生命周期应由 ctx 预算统一约束。
func TestAnthropicUpstreamClientHasNoClientLevelTimeout(t *testing.T) {
	if anthropicUpstreamClient.Timeout != 0 {
		t.Fatalf("Timeout = %s, want 0（流式 body 读取会被 client.Timeout 截断）",
			anthropicUpstreamClient.Timeout)
	}
}

// 必须是复用同一个实例，否则每次请求新建 client 会让连接池完全失效。
func TestAnthropicUpstreamClientIsSharedInstance(t *testing.T) {
	first := anthropicUpstreamClient
	// 通过多次读取确认是稳定的包级变量，而非每次调用生成。
	if anthropicUpstreamClient != first {
		t.Fatal("anthropicUpstreamClient 不是稳定实例")
	}
	// 与 provider 工厂产出的纪律应一致（同源）。
	want := provider.NewProviderHTTPClient(0)
	wantTransport, _ := want.Transport.(*http.Transport)
	gotTransport, _ := first.Transport.(*http.Transport)
	if wantTransport == nil || gotTransport == nil {
		t.Fatal("transport 类型断言失败")
	}
	if gotTransport.DisableCompression != wantTransport.DisableCompression ||
		gotTransport.ForceAttemptHTTP2 != wantTransport.ForceAttemptHTTP2 ||
		gotTransport.MaxIdleConnsPerHost != wantTransport.MaxIdleConnsPerHost {
		t.Errorf("anthropic 客户端传输纪律与 provider 工厂不一致:\n got  = %+v\n want = %+v",
			gotTransport, wantTransport)
	}
}
