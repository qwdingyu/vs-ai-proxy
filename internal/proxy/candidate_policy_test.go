package proxy

import (
	"testing"

	"github.com/dingyuwang/vs-ai-proxy/internal/config"
	"github.com/dingyuwang/vs-ai-proxy/internal/provider"
)

// ---------------------------------------------------------------------------
// applyDefenseCandidatePolicy：防止跨 provider 静默误路由
//
// 这是"绑定 provider 的模型会不会被悄悄路由到另一个 provider"的最后一道闸门。
// 3 个入口（/v1/chat/completions、/api/chat、/v1/messages）都依赖它。
// 一旦被误改为"返回全部候选"，请求会在首选 provider 失败后打到用户未预期的
// 供应商，造成错误计费与数据外流，且从响应上看不出来。
// ---------------------------------------------------------------------------

func testCandidates(n int) []provider.Candidate {
	out := make([]provider.Candidate, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, provider.Candidate{
			Provider:   &provider.ProviderEntry{Priority: i},
			UpstreamID: "upstream-" + string(rune('a'+i)),
			ModelID:    "model-" + string(rune('a'+i)),
			Priority:   i,
		})
	}
	return out
}

// 多个候选时必须只保留首选，避免跨 provider 兜底。
func TestApplyDefenseCandidatePolicy_KeepsOnlyFirst(t *testing.T) {
	cfg := &config.AppConfig{}

	for _, n := range []int{2, 3, 10} {
		got := applyDefenseCandidatePolicy(cfg, testCandidates(n))
		if len(got) != 1 {
			t.Fatalf("候选数 %d：返回 %d 个候选，want 1（跨 provider 兜底会把请求打到未预期的供应商）", n, len(got))
		}
		if got[0].UpstreamID != "upstream-a" {
			t.Fatalf("候选数 %d：保留的是 %q，want 首选 upstream-a", n, got[0].UpstreamID)
		}
	}
}

// 单个候选必须原样返回（不能被截断成空）。
func TestApplyDefenseCandidatePolicy_SingleCandidateUnchanged(t *testing.T) {
	cfg := &config.AppConfig{}
	got := applyDefenseCandidatePolicy(cfg, testCandidates(1))
	if len(got) != 1 || got[0].UpstreamID != "upstream-a" {
		t.Fatalf("单候选被改动: %+v", got)
	}
}

// 空候选必须返回空，不能 panic 也不能凭空造出候选。
func TestApplyDefenseCandidatePolicy_EmptyStaysEmpty(t *testing.T) {
	cfg := &config.AppConfig{}
	if got := applyDefenseCandidatePolicy(cfg, nil); len(got) != 0 {
		t.Fatalf("nil 候选返回 %d 个，want 0", len(got))
	}
	if got := applyDefenseCandidatePolicy(cfg, []provider.Candidate{}); len(got) != 0 {
		t.Fatalf("空候选返回 %d 个，want 0", len(got))
	}
}

// 即使 Defense.Enabled 显式关闭，也**不得**放开跨 provider 兜底。
// 这是文档化的设计决策：防御开关只控制 provider 内重试/冷却，不代表跨 provider fallback。
func TestApplyDefenseCandidatePolicy_IgnoresDefenseSwitch(t *testing.T) {
	enabled := false
	cfg := &config.AppConfig{}
	cfg.Defense.Enabled = &enabled

	got := applyDefenseCandidatePolicy(cfg, testCandidates(5))
	if len(got) != 1 {
		t.Fatalf("Defense.Enabled=false 时返回 %d 个候选，want 1（开关不应放开跨 provider 兜底）", len(got))
	}
}

// 首选必须是列表第一个，不能被重排（健康排序在更早阶段完成）。
func TestApplyDefenseCandidatePolicy_PreservesOrdering(t *testing.T) {
	cfg := &config.AppConfig{}
	in := testCandidates(4)
	got := applyDefenseCandidatePolicy(cfg, in)
	if len(got) != 1 {
		t.Fatalf("返回 %d 个，want 1", len(got))
	}
	if got[0].UpstreamID != in[0].UpstreamID {
		t.Fatalf("首选被重排：got %q, want %q", got[0].UpstreamID, in[0].UpstreamID)
	}
}
