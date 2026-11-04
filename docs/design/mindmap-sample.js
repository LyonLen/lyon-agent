// 验证样本：把「lyon-agent 思维导图功能」这场设计讨论蒸馏成一张图。
// 字段严格照 docs/design/mindmap.md §3 的节点模型，供后续 M1 直接复用当 fixture。
window.MAP = {
  title: "对话实时思维导图",
  subtitle: "lyon 目前对这次讨论的理解 · 有错请你改",
  revision: 1,
  nodes: [
    { id: "root", parent: null, label: "对话实时思维导图（mindmap）", kind: "branch", turn: 6, state: "confirmed" },

    // 1. 需求
    { id: "req", parent: "root", label: "需求", kind: "branch", turn: 5, state: "confirmed" },
    { id: "req-live", parent: "req", label: "随对话实时长出来的导图卡片", kind: "fact", turn: 6, state: "confirmed" },
    { id: "req-card", parent: "req", label: "住在对话流里，形制比照 plan 卡", kind: "fact", turn: 6, state: "confirmed" },
    { id: "req-edit", parent: "req", label: "图上的调整回流 chat，并自动提问", kind: "fact", turn: 8, state: "confirmed" },
    { id: "req-new", parent: "req", label: "全新方向，不复用 BACKLOG 候选", kind: "fact", turn: 5, state: "confirmed" },

    // 2. 机制
    { id: "mech", parent: "root", label: "机制选型", kind: "branch", turn: 7, state: "proposed" },
    { id: "mech-plan", parent: "mech", label: "plan 卡＝todo_write 全量重写＋无状态渲染", kind: "fact", turn: 7, state: "confirmed" },
    { id: "mech-no", parent: "mech", label: "不照抄：token 平方增长＋模型没动机重画", kind: "decision", turn: 7, state: "proposed" },
    { id: "mech-side", parent: "mech", label: "旁路蒸馏，挂 prompt_end tap", kind: "decision", turn: 7, state: "proposed" },
    { id: "mech-proto", parent: "mech", label: "新事件走 attrs 前进兼容位", kind: "fact", turn: 9, state: "confirmed" },

    // 3. 硬规则
    { id: "rule", parent: "root", label: "三条硬规则", kind: "branch", turn: 8, state: "proposed" },
    { id: "rule-pin", parent: "rule", label: "人工改动是权威，模型不得覆盖", kind: "decision", turn: 8, state: "proposed" },
    { id: "rule-auth", parent: "rule", label: "图上操作 ≠ 执行授权，不绕审批", kind: "decision", turn: 8, state: "proposed" },
    { id: "rule-ask", parent: "rule", label: "自动提问须防抖＋提示纪律", kind: "decision", turn: 8, state: "proposed" },
    { id: "rule-tomb", parent: "rule", label: "删除＝负信号（tombstone）", kind: "decision", turn: 8, state: "proposed" },

    // 4. 已拍板
    { id: "settled", parent: "root", label: "已拍板", kind: "branch", turn: 12, state: "confirmed" },
    { id: "settled-prop", parent: "settled", label: "图是「待你确认的理解」，不是结论", kind: "decision", turn: 12, state: "confirmed" },
    { id: "settled-vis", parent: "settled", label: "提案态虚线 / 确认态实线", kind: "decision", turn: 12, state: "proposed" },

    // 5. 风险
    { id: "risk", parent: "root", label: "风险", kind: "branch", turn: 11, state: "proposed" },
    { id: "risk-second", parent: "risk", label: "二手阅读长得像一手结论（已由提案态对冲）", kind: "fact", turn: 11, state: "proposed" },
    { id: "risk-never", parent: "risk", label: "永远没有完成态 → 会没人看", kind: "open_question", turn: 11, state: "proposed" },
    { id: "risk-flicker", parent: "risk", label: "「能动」与「不闪」天生矛盾", kind: "open_question", turn: 11, state: "proposed" },
    { id: "risk-interrupt", parent: "risk", label: "自动提问被打断感杀死", kind: "open_question", turn: 11, state: "proposed" },
    { id: "risk-cost", parent: "risk", label: "成本是长期持续项", kind: "fact", turn: 11, state: "proposed" },

    // 6. 亮点
    { id: "plus", parent: "root", label: "亮点补充", kind: "branch", turn: 11, state: "proposed" },
    { id: "plus-handoff", parent: "plus", label: "图当会话交接物：存 space，下段注入", kind: "decision", turn: 11, state: "proposed" },
    { id: "plus-exec", parent: "plus", label: "节点＝可执行单元，与大规划合流", kind: "decision", turn: 11, state: "proposed" },
    { id: "plus-eval", parent: "plus", label: "图的变化序列＝可评分的抓重点数据", kind: "decision", turn: 11, state: "proposed" },

    // 7. 未决
    { id: "open", parent: "root", label: "未决", kind: "branch", turn: 12, state: "proposed" },
    { id: "open-model", parent: "open", label: "蒸馏用哪个模型", kind: "open_question", turn: 12, state: "proposed" },
    { id: "open-auto", parent: "open", label: "自动提问默认开还是关", kind: "open_question", turn: 12, state: "proposed" },
    { id: "open-scope", parent: "open", label: "总开关是否 web 开、TUI 关", kind: "open_question", turn: 12, state: "proposed" },

    // 8. 下一步
    { id: "next", parent: "root", label: "下一步", kind: "branch", turn: 12, state: "proposed" },
    { id: "next-validate", parent: "next", label: "先用真实会话验价值（本图）", kind: "decision", turn: 12, state: "confirmed" },
    { id: "next-m1", parent: "next", label: "M1 见证版只读卡片", kind: "decision", turn: 12, state: "proposed" },
    { id: "next-m2", parent: "next", label: "M2 人工通道（核心）", kind: "decision", turn: 12, state: "proposed" }
  ]
};