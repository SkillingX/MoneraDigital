package safeheron

// AML 风险等级聚合值（来自 amlList 中所有 provider 的最高严重度）。
// 用作 SummarizeAmlRiskLevel 的返回值；deposit 侧 deposits.aml_risk_level 和
// approval 侧 approval_records.aml_risk_level 都直接落库这些字符串。
const (
	AmlRiskLow     = "LOW"
	AmlRiskMedium  = "MEDIUM"
	AmlRiskHigh    = "HIGH"
	AmlRiskSevere  = "SEVERE"
	AmlRiskUnknown = "UNKNOWN"
	AmlRiskFailed  = "FAILED"
	AmlRiskSkipped = "SKIPPED"
	AmlRiskPending = "PENDING"
	AmlRiskEmpty   = "EMPTY"
)

// amlRiskSeverity 返回风险等级的数值严重度，数值越大越严重。
func amlRiskSeverity(level string) int {
	switch level {
	case AmlRiskLow:
		return 1
	case AmlRiskMedium:
		return 2
	case AmlRiskUnknown:
		return 3
	case AmlRiskHigh:
		return 4
	case AmlRiskSevere:
		return 5
	default:
		return 3 // 未知 riskLevel 视同 UNKNOWN
	}
}

// SummarizeAmlRiskLevel 取 amlList 中所有 provider 的最高严重度。
// 优先级：PENDING > FAILED > SKIPPED > riskLevel 比较。
// 调用方：deposit.DecideKYT、approval.DecideSweepAML。
func SummarizeAmlRiskLevel(amlList []AmlReport) string {
	if len(amlList) == 0 {
		return AmlRiskEmpty
	}
	hasPending := false
	hasFailed := false
	hasSkipped := false
	hasCompleted := false
	maxSev := 0
	maxLevel := AmlRiskUnknown

	for _, r := range amlList {
		switch r.Status {
		case "PENDING":
			hasPending = true
		case "FAILED":
			hasFailed = true
		case "SKIPPED":
			hasSkipped = true
		case "COMPLETED":
			hasCompleted = true
			sev := amlRiskSeverity(r.RiskLevel)
			if sev > maxSev {
				maxSev = sev
				maxLevel = r.RiskLevel
			}
		}
	}

	if hasPending {
		return AmlRiskPending
	}
	if hasFailed {
		return AmlRiskFailed
	}
	if hasSkipped {
		return AmlRiskSkipped
	}
	if !hasCompleted {
		return AmlRiskUnknown
	}
	return maxLevel
}
