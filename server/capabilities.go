package server

import (
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 能力位（TD §9 六页共用；调研 §8.3 的跨页前置 1）
//
// 六页里有三页依赖**可选**端口：本体的 OntologyReader、维护面的 Maintainer、
// 存储健康的 HealthReader（P6-4 引入）。缺端口时最坏的做法是回一个空列表 ——
// 页面会显示"暂无数据"，把"后端答不了"伪装成"库里没有"。这一层负责把
// "有没有这个能力"显式说出来。
//
// 判据必须是**类型断言**（端口是否真的被注入），不是配置开关：配置可以写
// true 而端口为 nil，那就又变成静默了。
// ===========================================================================

// capabilities 计算当前装配的能力位。
//
// 每个 false 都必须在 Reasons 里留一条原因（验收：缺端口时显式 false + reason，
// 不得静默）。为 true 的能力不写条目，避免把"一切正常"也变成噪音。
func (s *Server) capabilities() Capabilities {
	caps := Capabilities{
		RecallPlan: s.recaller != nil,
		Audit:      s.maint != nil,
		Ontology:   s.hasOntologyReader(),
		// Health 依赖一个尚未定义的可选端口（P6-4 的 store.HealthReader）。
		// 在它落地之前恒为 false，并且必须说清楚原因——这正是本字段存在的理由。
		Health: false,
	}
	if s.recaller == nil {
		caps.Reasons = append(caps.Reasons,
			"recall_plan: 未注入 recall.Recaller，/api/recall 不可用（要解释召回必须先有召回）")
	}
	if s.maint == nil {
		caps.Reasons = append(caps.Reasons,
			"audit: 未注入 ops.Maintainer，审计/台账不可读（写入口与台账同源，缺它就没有可读的 op 记录）")
	}
	if !caps.Ontology {
		caps.Reasons = append(caps.Reasons,
			"ontology: 后端未实现 store.OntologyReader，概念/别名无法枚举（游标对齐与本体页都会因此降级）")
	}
	caps.Reasons = append(caps.Reasons,
		"health: 尚无 store.HealthReader 可选端口（sqlite.Health 在 sqlite 包内，server 按分层不得 import 它）；"+
			"该能力计划随 P6-4 Metrics 引入")
	return caps
}

// hasOntologyReader 判断存储端口是否实现了本体枚举能力。
//
// 与 recall.New 的自动探测同判据（store.OntologyReader），因此"召回能对齐本体"
// 与"前端认为本体可用"不会给出相反答案。
func (s *Server) hasOntologyReader() bool {
	if s.ports.Store == nil {
		return false
	}
	_, ok := s.ports.Store.(store.OntologyReader)
	return ok
}
