package deposit

import (
	"strconv"
	"time"

	"monera-digital/internal/safeheron"
)

// KYT 风险等级常量已迁移到 safeheron 包（v1.1 §13.5.1）。
// 调用方请使用 safeheron.AmlRiskLow / AmlRiskMedium / ... / AmlRiskEmpty。

// KYT 失败原因码（deposits.failed_reason 列）。
// 初查路径不带后缀；超时兜底路径带 _AFTER_TIMEOUT，运营一眼可区分。
const (
	ReasonKytUntriggered    = "KYT_UNTRIGGERED"
	ReasonKytRiskPrefix     = "KYT_RISK_"
	ReasonKytProviderFailed = "KYT_PROVIDER_FAILED"
	ReasonKytSkipped        = "KYT_SKIPPED"

	ReasonKytUntriggeredAfterTimeout    = "KYT_UNTRIGGERED_AFTER_TIMEOUT"
	ReasonKytProviderFailedAfterTimeout = "KYT_PROVIDER_FAILED_AFTER_TIMEOUT"
	ReasonKytSkippedAfterTimeout        = "KYT_SKIPPED_AFTER_TIMEOUT"
	ReasonKytTimeoutStillPending        = "KYT_TIMEOUT_STILL_PENDING"

	ReasonKytOrphanAlert  = "KYT_ORPHAN_ALERT"
	ReasonKytApiFailed    = "KYT_API_FAILED"
	ReasonKytEmptyAmlList = "KYT_EMPTY_AML_LIST"
	ReasonKytUnknownState = "KYT_UNKNOWN_STATE"
)

func BuildKytRiskReason(riskLevel string) string {
	return ReasonKytRiskPrefix + riskLevel
}

func BuildKytTimeoutRiskReason(riskLevel string) string {
	return ReasonKytRiskPrefix + riskLevel + "_AFTER_TIMEOUT"
}

// AlertLevelForKyt 把 KYT 风险等级映射到告警级别（K-17）。
func AlertLevelForKyt(riskLevel string) string {
	switch riskLevel {
	case safeheron.AmlRiskHigh, safeheron.AmlRiskSevere:
		return "ERROR"
	default:
		return "WARN"
	}
}

type KytDecisionAction int

const (
	KytActionCredit       KytDecisionAction = iota // CREDITED
	KytActionKeepPending                           // 保持 KYT_PENDING
	KytActionManualReview                          // MANUAL_REVIEW
)

type KytDecision struct {
	Action     KytDecisionAction
	RiskLevel  string
	Reason     string
	AlertLevel string
}

// DecideKYT 根据 amlScreeningState 和 amlList 决定下一步（SPEC §6.5.1 处置矩阵）。
// isAfterTimeout=true 时 reason 带 _AFTER_TIMEOUT 后缀。
func DecideKYT(state string, amlList []safeheron.AmlReport, isAfterTimeout bool) KytDecision {
	switch state {
	case "IN_PROGRESS":
		if isAfterTimeout {
			return KytDecision{
				Action:     KytActionManualReview,
				RiskLevel:  safeheron.AmlRiskPending,
				Reason:     ReasonKytTimeoutStillPending,
				AlertLevel: "ERROR",
			}
		}
		return KytDecision{Action: KytActionKeepPending, RiskLevel: safeheron.AmlRiskPending}

	case "UNTRIGGERED":
		reason := ReasonKytUntriggered
		if isAfterTimeout {
			reason = ReasonKytUntriggeredAfterTimeout
		}
		return KytDecision{
			Action:     KytActionManualReview,
			RiskLevel:  safeheron.AmlRiskUnknown,
			Reason:     reason,
			AlertLevel: "WARN",
		}

	case "TRIGGERED":
		// fall through to risk evaluation below

	default:
		return KytDecision{
			Action:     KytActionManualReview,
			RiskLevel:  safeheron.AmlRiskUnknown,
			Reason:     ReasonKytUnknownState,
			AlertLevel: "ERROR",
		}
	}

	risk := safeheron.SummarizeAmlRiskLevel(amlList)
	switch risk {
	case safeheron.AmlRiskEmpty:
		return KytDecision{
			Action:     KytActionManualReview,
			RiskLevel:  safeheron.AmlRiskEmpty,
			Reason:     ReasonKytEmptyAmlList,
			AlertLevel: "WARN",
		}
	case safeheron.AmlRiskPending:
		if isAfterTimeout {
			return KytDecision{
				Action:     KytActionManualReview,
				RiskLevel:  safeheron.AmlRiskPending,
				Reason:     ReasonKytTimeoutStillPending,
				AlertLevel: "ERROR",
			}
		}
		return KytDecision{Action: KytActionKeepPending, RiskLevel: safeheron.AmlRiskPending}
	case safeheron.AmlRiskLow:
		return KytDecision{Action: KytActionCredit, RiskLevel: safeheron.AmlRiskLow}
	case safeheron.AmlRiskFailed:
		reason := ReasonKytProviderFailed
		if isAfterTimeout {
			reason = ReasonKytProviderFailedAfterTimeout
		}
		return KytDecision{
			Action:     KytActionManualReview,
			RiskLevel:  safeheron.AmlRiskFailed,
			Reason:     reason,
			AlertLevel: "WARN",
		}
	case safeheron.AmlRiskSkipped:
		reason := ReasonKytSkipped
		if isAfterTimeout {
			reason = ReasonKytSkippedAfterTimeout
		}
		return KytDecision{
			Action:     KytActionManualReview,
			RiskLevel:  safeheron.AmlRiskSkipped,
			Reason:     reason,
			AlertLevel: "WARN",
		}
	default:
		// MEDIUM / HIGH / SEVERE / UNKNOWN
		reason := BuildKytRiskReason(risk)
		if isAfterTimeout {
			reason = BuildKytTimeoutRiskReason(risk)
		}
		return KytDecision{
			Action:     KytActionManualReview,
			RiskLevel:  risk,
			Reason:     reason,
			AlertLevel: AlertLevelForKyt(risk),
		}
	}
}

// maxLastUpdateTime 取 amlList 中 LastUpdateTime（UNIX 毫秒字符串）的最大值。
// 全部解析失败时返回零值 time.Time{} —— S-3：之前回退到 time.Now() 会让
// LockOneKYTPendingTimeout 永远捞不到这条 deposit（updated_at < NOW()-20m
// 永远不成立），导致永久卡 KYT_PENDING。返回零值后超时扫描会立即捡到。
func maxLastUpdateTime(amlList []safeheron.AmlReport) time.Time {
	var max int64
	for _, r := range amlList {
		if ts, err := strconv.ParseInt(r.LastUpdateTime, 10, 64); err == nil && ts > max {
			max = ts
		}
	}
	if max == 0 {
		return time.Time{}
	}
	return time.UnixMilli(max)
}
