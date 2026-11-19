package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/tim5wang/gmem/extract"
	"github.com/tim5wang/gmem/llm"
	"github.com/tim5wang/gmem/memory"
	"github.com/tim5wang/gmem/server"
	"github.com/tim5wang/gmem/store"
)

// Options 是 Server 的装配参数。
type Options struct {
	// Service 是动词面（必需）。
	Service *memory.Service
	// Graph 是图/实体/裁决的聚合面（必需：gmem_graph 与 gmem_review 必须复用它）。
	Graph *server.Server
	// LLMConfig 是 GMEM_LLM_* 的解析结果（D8 的配置优先那一半）。
	//
	// 这里收**配置**而不是收**结论**：sampling 能力只有在 initialize 之后
	// 才知道（客户端在 initialize 里声明 capabilities.sampling）。若在构造时
	// 就把策略定死，客户端后来声明了 sampling 也用不上——那正是 D8 最容易
	// 做错的地方。
	LLMConfig llm.Config
	// LLMClient 是可注入的 LLM 客户端（宿主嵌入时用；CLI 下恒为 nil）。
	LLMClient extract.LLMClient
	// In / Out 是协议流（stdio）。**Out 只放 JSON-RPC 报文**。
	In  io.Reader
	Out io.Writer
	// Log 是日志流（必须是 stderr）。协议流上任何多余输出都会破坏协议。
	Log io.Writer
	// Scope 是缺省 scope（工具参数没给时用它）。
	Scope store.Scope
	// OnSampler 在构造完成后回传 sampler 实例（可选）。
	//
	// 为什么用回调而不是导出字段：sampler 需要持有本服务的写锁与回包投递表，
	// 只能由 New 内部创建。调用方（cmd/gmem）需要拿它去装配延迟抽取器。
	OnSampler func(llm.Sampler)
}

// Server 是 MCP stdio 服务端。
type Server struct {
	svc       *memory.Service
	graph     *server.Server
	llmCfg    llm.Config
	llmClient extract.LLMClient
	in        io.Reader
	out       io.Writer
	log       io.Writer
	scope     store.Scope

	// writeMu 保护 Out：并发处理请求时不得交错写半个报文。
	writeMu sync.Mutex

	// sampling 是客户端能力与回包投递。
	sampling *peerSampler

	// clientInfo 记录 initialize 里的客户端信息（doctor / 日志用）。
	clientInfo string
}

// New 构造服务端。
func New(opts Options) (*Server, error) {
	if opts.Service == nil {
		return nil, errors.New("mcp: nil service")
	}
	if opts.Graph == nil {
		return nil, errors.New("mcp: nil graph server（gmem_graph / gmem_review 需要它）")
	}
	if opts.In == nil || opts.Out == nil {
		return nil, errors.New("mcp: nil In/Out（stdio 传输需要两条流）")
	}
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	if opts.Scope == "" {
		opts.Scope = store.ScopeUser
	}
	s := &Server{
		svc: opts.Service, graph: opts.Graph,
		llmCfg: opts.LLMConfig, llmClient: opts.LLMClient,
		in: opts.In, out: opts.Out, log: opts.Log, scope: opts.Scope,
	}
	s.sampling = newPeerSampler(s)
	if opts.OnSampler != nil {
		opts.OnSampler(s.sampling)
	}
	return s, nil
}

// logf 把日志写到 Log（**绝不写 Out**：stdio 上的任何多余输出都会破坏协议）。
func (s *Server) logf(format string, args ...any) {
	fmt.Fprintf(s.log, format+"\n", args...)
}

// Serve 读取并处理报文，直到输入结束或 ctx 取消。
//
// # 为什么每个请求一个 goroutine
//
// tools/call 里可能发生 **服务端→客户端** 的 sampling 往返（§3.3 的兜底路径）。
// 若在读取循环里同步处理，那条请求就会卡在"等服务端自己读下一条报文"上——死锁。
// 因此读取循环只做分发：请求交给 goroutine，响应投递给等待中的 sampling 调用。
//
// 代价是响应可能乱序；JSON-RPC 用 id 配对，乱序是合法的。
func (s *Server) Serve(ctx context.Context) error {
	r := bufio.NewReaderSize(s.in, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			s.handleLine(ctx, line)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("mcp: 读取 stdio: %w", err)
		}
	}
}

// handleLine 处理一行报文。
//
// 解析失败 → -32700；不 panic、不中断循环（一个坏报文不该让整个服务挂掉）。
func (s *Server) handleLine(ctx context.Context, line []byte) {
	var raw json.RawMessage
	if !json.Valid(trimJSON(line)) {
		s.write(newError(nil, &RPCError{Code: CodeParseError, Message: "报文不是合法 JSON"}))
		return
	}
	raw = trimJSON(line)

	var req Request
	if err := json.Unmarshal(raw, &req); err != nil {
		s.write(newError(nil, &RPCError{Code: CodeInvalidRequest, Message: "报文结构非法: " + err.Error()}))
		return
	}
	// 客户端对我们发出的 sampling 请求的回包。
	if req.IsResponse() {
		s.sampling.complete(req)
		return
	}
	if req.Method == "" {
		s.write(newError(req.ID, &RPCError{Code: CodeInvalidRequest, Message: "缺少 method"}))
		return
	}
	if req.IsNotification() {
		// 通知不回响应（协议要求）。目前只接受 notifications/initialized 这类。
		s.logf("mcp: 收到通知 %s（无需响应）", req.Method)
		return
	}
	go func() {
		resp := s.dispatch(ctx, req)
		s.write(resp)
	}()
}

// dispatch 执行一次请求并产出响应。
func (s *Server) dispatch(ctx context.Context, req Request) Response {
	switch req.Method {
	case "initialize":
		return s.handleInitialize(req)
	case "tools/list":
		return newResult(req.ID, map[string]any{"tools": s.toolList()})
	case "tools/call":
		return s.handleToolCall(ctx, req)
	case "ping":
		return newResult(req.ID, map[string]any{})
	default:
		return newError(req.ID, methodNotFound(req.Method))
	}
}

// InitializeResult 是 initialize 的返回体。
type InitializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      map[string]any `json:"serverInfo"`
	// Instructions 是给人看的一段话（含别名层的免责声明）。
	Instructions string `json:"instructions,omitempty"`
}

// handleInitialize 完成握手，并记录客户端声明的 sampling 能力。
func (s *Server) handleInitialize(req Request) Response {
	var params struct {
		ProtocolVersion string `json:"protocolVersion"`
		ClientInfo      *struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"clientInfo"`
		Capabilities *struct {
			Sampling *struct{} `json:"sampling"`
		} `json:"capabilities"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return newError(req.ID, invalidParams("initialize 参数非法: %v", err))
		}
	}
	// sampling 能力：只有客户端**显式声明**才算可用（D8 的兜底前提）。
	capable := params.Capabilities != nil && params.Capabilities.Sampling != nil
	s.sampling.setCapable(capable)
	if params.ClientInfo != nil {
		s.clientInfo = fmt.Sprintf("%s/%s", params.ClientInfo.Name, params.ClientInfo.Version)
	}
	if capable {
		s.logf("mcp: 客户端声明了 sampling 能力（LLM 可走零密钥兜底）")
	} else {
		s.logf("mcp: 客户端未声明 sampling 能力（抽取将降级为 L0+L1，除非配置 GMEM_LLM_*）")
	}
	version := params.ProtocolVersion
	if version == "" {
		version = "2024-11-05"
	}
	return newResult(req.ID, InitializeResult{
		ProtocolVersion: version,
		Capabilities: map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
		ServerInfo: map[string]any{"name": "gmem", "version": "0.5.0"},
		Instructions: "Gmem 是单体 Agent 记忆系统。" +
			"兼容别名层（create_entities / create_relations / add_observations / search_nodes / open_nodes / read_graph）" +
			"只做**能力映射**，不承诺与 MCP 官方 memory server 的 JSON 存储格式互通。" +
			"抽取能力：" + string(s.strategy().Mode) + "。",
	})
}

// strategy 按**当前**的客户端能力解析 LLM 策略（D8）。
//
// 每次调用都重算而不是缓存：initialize 之前 sampling 能力未知（按不可用处理），
// initialize 之后才可能变为可用。缓存会让"客户端声明了 sampling 却仍报降级"
// 这种矛盾出现——而降级声明一旦说谎，使用者就没有别的渠道发现。
func (s *Server) strategy() llm.Strategy {
	var sampler llm.Sampler
	if s.sampling != nil {
		sampler = s.sampling
	}
	return llm.Resolve(s.llmCfg, s.llmClient, sampler)
}

// write 序列化并写出一条报文（加锁，避免并发交错）。
func (s *Server) write(resp Response) {
	body, err := json.Marshal(resp)
	if err != nil {
		// 序列化失败时至少给出一条合法的错误报文，绝不写半个 JSON。
		body, _ = json.Marshal(Response{JSONRPC: "2.0", ID: resp.ID,
			Error: &RPCError{Code: CodeInternalError, Message: "响应序列化失败: " + err.Error()}})
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, _ = s.out.Write(append(body, '\n'))
}

// writeRequest 是**服务端→客户端**的请求（sampling 用）。
func (s *Server) writeRequest(id string, method string, params any) error {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	})
	if err != nil {
		return fmt.Errorf("mcp: 序列化请求: %w", err)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err = s.out.Write(append(body, '\n'))
	return err
}

// trimJSON 去掉行首尾的空白（含 \r\n）。
func trimJSON(b []byte) []byte {
	start, end := 0, len(b)
	for start < end {
		switch b[start] {
		case ' ', '\t', '\r', '\n':
			start++
			continue
		}
		break
	}
	for end > start {
		switch b[end-1] {
		case ' ', '\t', '\r', '\n':
			end--
			continue
		}
		break
	}
	return b[start:end]
}
