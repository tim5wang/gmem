package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/tim5wang/gmem/llm"
)

// ===========================================================================
// sampling 对端：服务端 → 客户端的 LLM 请求（D8 的零密钥兜底）
//
// # 协议要点
//
// 服务端发 `sampling/createMessage`，客户端弹窗/选模型/调 LLM，再把结果回包。
// 因此这里必须能**一边等回包、一边继续读下一条报文**——读取循环因此把请求
// 分派到 goroutine（见 server.go 的 Serve），回包按 id 投递到等待中的调用。
//
// # 人在环路（§3.3 第 1 条）
//
// sampling 可能弹多个确认框。因此本实现**只在批量写入时问一次**（由上层调用
// 一次 Complete 覆盖整批），单条低价值写入直接降级而不弹窗——那部分策略在
// 上层（mcp 的工具处理里），这里只保证"一次请求一次回包"。
// ===========================================================================

// peerSampler 实现对客户端 sampling 能力的调用。
type peerSampler struct {
	srv *Server

	mu      sync.RWMutex
	capable bool
	pending map[string]chan Request
	seq     atomic.Int64
}

func newPeerSampler(s *Server) *peerSampler {
	return &peerSampler{srv: s, pending: map[string]chan Request{}}
}

// setCapable 记录客户端是否声明了 sampling 能力（initialize 时置位）。
func (p *peerSampler) setCapable(v bool) {
	p.mu.Lock()
	p.capable = v
	p.mu.Unlock()
}

// SamplingAvailable 实现 llm.Sampler。
func (p *peerSampler) SamplingAvailable() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.capable
}

// CreateMessage 实现 llm.Sampler：发一条 sampling 请求并等回包。
func (p *peerSampler) CreateMessage(ctx context.Context, req llm.SamplingRequest) (string, error) {
	if !p.SamplingAvailable() {
		return "", fmt.Errorf("mcp: sampling 不可用（客户端未声明能力）")
	}
	id := fmt.Sprintf("gmem-sampling-%d", p.seq.Add(1))
	ch := make(chan Request, 1)
	p.mu.Lock()
	p.pending[id] = ch
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
	}()

	params := map[string]any{
		"messages": []map[string]any{{
			"role":    "user",
			"content": map[string]any{"type": "text", "text": req.Prompt},
		}},
		"maxTokens": req.MaxTokens,
		// 模型偏好用**优先级**而不是模型名：服务端不知道该客户端有什么模型。
		"modelPreferences": map[string]any{
			"intelligencePriority": req.IntelligencePriority,
			"speedPriority":        req.SpeedPriority,
			"costPriority":         req.CostPriority,
		},
	}
	if req.System != "" {
		params["systemPrompt"] = req.System
	}
	if req.Temperature > 0 {
		params["temperature"] = req.Temperature
	}
	if err := p.srv.writeRequest(id, "sampling/createMessage", params); err != nil {
		return "", err
	}

	select {
	case <-ctx.Done():
		return "", fmt.Errorf("mcp: sampling 被取消: %w", ctx.Err())
	case resp := <-ch:
		if resp.Error != nil {
			return "", fmt.Errorf("mcp: 客户端拒绝 sampling: %s", resp.Error.Message)
		}
		return samplingText(resp.Result)
	}
}

// complete 投递一条回包（由读取循环调用）。
func (p *peerSampler) complete(req Request) {
	var id string
	if err := json.Unmarshal(req.ID, &id); err != nil {
		// id 可能是数字：统一按原始字节做键，避免"字符串/数字"两种写法对不上。
		id = string(req.ID)
	}
	if id == "" {
		id = string(req.ID)
	}
	p.mu.RLock()
	ch, ok := p.pending[id]
	if !ok {
		ch, ok = p.pending[string(req.ID)]
	}
	p.mu.RUnlock()
	if !ok {
		p.srv.logf("mcp: 收到无法配对的回包 id=%s（已丢弃）", string(req.ID))
		return
	}
	select {
	case ch <- req:
	default:
		// 缓冲为 1，正常不会走到这里；走到说明同一 id 回了两次。
		p.srv.logf("mcp: 回包 id=%s 重复（已丢弃）", string(req.ID))
	}
}

// samplingText 从回包里抽出文本。
//
// 兼容两种形状：result.content 是 {type:text,text} 对象（spec 形态），
// 或 result.content 直接是字符串（部分客户端的简化写法）。
func samplingText(result json.RawMessage) (string, error) {
	if len(result) == 0 {
		return "", fmt.Errorf("mcp: sampling 回包缺少 result")
	}
	var payload struct {
		Content json.RawMessage `json:"content"`
		Text    string          `json:"text"`
	}
	if err := json.Unmarshal(result, &payload); err != nil {
		return "", fmt.Errorf("mcp: sampling 回包结构非法: %w", err)
	}
	if payload.Text != "" {
		return payload.Text, nil
	}
	if len(payload.Content) == 0 {
		return "", fmt.Errorf("mcp: sampling 回包没有 content")
	}
	var block struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(payload.Content, &block); err == nil && block.Text != "" {
		return block.Text, nil
	}
	var text string
	if err := json.Unmarshal(payload.Content, &text); err == nil {
		return text, nil
	}
	// content 是数组形态时的兜底：取所有 text 块拼接。
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(payload.Content, &blocks); err == nil {
		out := ""
		for _, b := range blocks {
			out += b.Text
		}
		if out != "" {
			return out, nil
		}
	}
	return "", fmt.Errorf("mcp: sampling 回包的 content 无法解析为文本")
}

// 编译期断言：peerSampler 满足 llm.Sampler。
var _ llm.Sampler = (*peerSampler)(nil)
