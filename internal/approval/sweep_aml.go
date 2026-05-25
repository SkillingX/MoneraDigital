package approval

import (
	"encoding/json"

	"monera-digital/internal/safeheron"
)

// SweepAMLDecision 是归集 AML 校验的结果（v1.1 Phase 1）。
// Phase 2 将扩展第三种状态（REJECT-overridable），用于运营审核覆盖。
type SweepAMLDecision struct {
	Approve   bool
	RiskLevel string // 写入 approval_records.aml_risk_level
	Reason    string // 写入 approval_records.reason 的子串
}

// DecideSweepAML 按 callback 携带的 AML 状态 + amlList 决定是否放行归集。
// Phase 1 策略（fail closed）：
//   - state != "TRIGGERED" → REJECT
//   - amlList 缺失 / 非法 / 解析失败 → REJECT
//   - SummarizeAmlRiskLevel(amlList) == "LOW" → APPROVE
//   - 其他风险等级 → REJECT
//
// 参见 docs/spec/approval-callback-spec.md §13.5。
func DecideSweepAML(state string, amlListRaw json.RawMessage) SweepAMLDecision {
	if state != "TRIGGERED" {
		label := state
		if label == "" {
			label = "MISSING"
		}
		return SweepAMLDecision{
			Approve:   false,
			RiskLevel: "STATE_" + label,
			Reason:    "SWEEP_AML_STATE_" + label,
		}
	}

	var amlList []safeheron.AmlReport
	if len(amlListRaw) > 0 {
		if err := json.Unmarshal(amlListRaw, &amlList); err != nil {
			return SweepAMLDecision{
				Approve:   false,
				RiskLevel: "PARSE_FAILED",
				Reason:    "SWEEP_AML_PARSE_FAILED",
			}
		}
	}

	risk := safeheron.SummarizeAmlRiskLevel(amlList)
	if risk == safeheron.AmlRiskLow {
		return SweepAMLDecision{
			Approve:   true,
			RiskLevel: risk,
			Reason:    "SWEEP_AML_OK",
		}
	}
	return SweepAMLDecision{
		Approve:   false,
		RiskLevel: risk,
		Reason:    "SWEEP_AML_RISK_" + risk,
	}
}
