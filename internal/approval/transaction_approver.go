package approval

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"monera-digital/internal/safeheron"
	walletconfig "monera-digital/internal/wallet/config"
)

type ChainLookup interface {
	GetCoinChainBySafeheronKey(key string) (*walletconfig.CoinChain, bool)
}

type TransactionApprover struct {
	config   ApprovalConfig
	registry ChainLookup
}

func NewTransactionApprover(cfg ApprovalConfig, registry ChainLookup) *TransactionApprover {
	return &TransactionApprover{config: cfg, registry: registry}
}

func (a *TransactionApprover) Evaluate(_ context.Context, biz *safeheron.CoSignerBizContentV3) (*ApprovalDecision, error) {
	var detail safeheron.TransactionApproval
	if err := json.Unmarshal(biz.Detail, &detail); err != nil {
		return &ApprovalDecision{
			Action: "REJECT",
			Reason: fmt.Sprintf("failed to parse transaction detail: %v", err),
		}, nil
	}

	if !a.isTxTypeAllowed(detail.TransactionType) {
		return &ApprovalDecision{
			Action: "REJECT",
			Reason: fmt.Sprintf("tx_type %q not in allowed list", detail.TransactionType),
		}, nil
	}

	switch detail.TransactionType {
	case "AUTO_SWEEP":
		return a.evaluateAutoSweep(&detail)
	case "AUTO_FUEL":
		return a.evaluateAutoFuel(&detail)
	case "UTXO_COLLECTION":
		return a.evaluateUTXOCollection(&detail)
	case "NORMAL":
		return &ApprovalDecision{
			Action: "REJECT",
			Reason: "NORMAL transactions not yet supported (reserved for withdrawal)",
		}, nil
	default:
		return &ApprovalDecision{
			Action: "REJECT",
			Reason: fmt.Sprintf("unknown transaction type: %s", detail.TransactionType),
		}, nil
	}
}

func (a *TransactionApprover) evaluateAutoSweep(detail *safeheron.TransactionApproval) (*ApprovalDecision, error) {
	if detail.DestinationAccountType != "VAULT_ACCOUNT" {
		return &ApprovalDecision{
			Action: "REJECT",
			Reason: fmt.Sprintf("AUTO_SWEEP destination must be VAULT_ACCOUNT, got %q", detail.DestinationAccountType),
		}, nil
	}
	if !a.isTargetAccountAllowed(detail.DestinationAccountKey) {
		return &ApprovalDecision{
			Action: "REJECT",
			Reason: fmt.Sprintf("AUTO_SWEEP destination account %q not in whitelist", detail.DestinationAccountKey),
		}, nil
	}
	// v1.1 Phase 1：AML 校验（D-AML-3 fail closed）。
	// 仅 TRIGGERED + LOW 放行，其余拒绝。详见 spec §13。
	amlDecision := DecideSweepAML(detail.AmlScreeningTriggeredState, detail.AmlList)
	if !amlDecision.Approve {
		return &ApprovalDecision{
			Action:       "REJECT",
			Reason:       fmt.Sprintf("AUTO_SWEEP rejected: %s (risk=%s)", amlDecision.Reason, amlDecision.RiskLevel),
			AmlRiskLevel: amlDecision.RiskLevel,
		}, nil
	}
	return &ApprovalDecision{
		Action:       "APPROVE",
		Reason:       fmt.Sprintf("AUTO_SWEEP approved (AML=%s)", amlDecision.RiskLevel),
		AmlRiskLevel: amlDecision.RiskLevel,
	}, nil
}

func (a *TransactionApprover) evaluateAutoFuel(detail *safeheron.TransactionApproval) (*ApprovalDecision, error) {
	if detail.DestinationAccountType != "VAULT_ACCOUNT" {
		return &ApprovalDecision{
			Action: "REJECT",
			Reason: fmt.Sprintf("AUTO_FUEL destination must be VAULT_ACCOUNT, got %q", detail.DestinationAccountType),
		}, nil
	}
	if !a.isTargetAccountAllowed(detail.DestinationAccountKey) {
		return &ApprovalDecision{
			Action: "REJECT",
			Reason: fmt.Sprintf("AUTO_FUEL destination account %q not in whitelist", detail.DestinationAccountKey),
		}, nil
	}
	// TODO: 金额校验待测试环境验证真实数据后补充 — spec §4.3
	return &ApprovalDecision{Action: "APPROVE", Reason: "AUTO_FUEL approved"}, nil
}

func (a *TransactionApprover) evaluateUTXOCollection(detail *safeheron.TransactionApproval) (*ApprovalDecision, error) {
	if detail.DestinationAccountType != "VAULT_ACCOUNT" {
		return &ApprovalDecision{
			Action: "REJECT",
			Reason: fmt.Sprintf("UTXO_COLLECTION destination must be VAULT_ACCOUNT, got %q", detail.DestinationAccountType),
		}, nil
	}
	if !a.isTargetAccountAllowed(detail.DestinationAccountKey) {
		return &ApprovalDecision{
			Action: "REJECT",
			Reason: fmt.Sprintf("UTXO_COLLECTION destination account %q not in whitelist", detail.DestinationAccountKey),
		}, nil
	}
	// v1.1 Phase 1：AML 校验，与 AUTO_SWEEP 同策略。
	amlDecision := DecideSweepAML(detail.AmlScreeningTriggeredState, detail.AmlList)
	if !amlDecision.Approve {
		return &ApprovalDecision{
			Action:       "REJECT",
			Reason:       fmt.Sprintf("UTXO_COLLECTION rejected: %s (risk=%s)", amlDecision.Reason, amlDecision.RiskLevel),
			AmlRiskLevel: amlDecision.RiskLevel,
		}, nil
	}
	return &ApprovalDecision{
		Action:       "APPROVE",
		Reason:       fmt.Sprintf("UTXO_COLLECTION approved (AML=%s)", amlDecision.RiskLevel),
		AmlRiskLevel: amlDecision.RiskLevel,
	}, nil
}

func (a *TransactionApprover) isTxTypeAllowed(txType string) bool {
	for _, allowed := range a.config.AllowedTxTypes {
		if allowed == txType {
			return true
		}
	}
	return false
}

func (a *TransactionApprover) isTargetAccountAllowed(accountKey string) bool {
	if len(a.config.SweepTargetAccounts) == 0 {
		return false
	}
	for _, allowed := range a.config.SweepTargetAccounts {
		if allowed == accountKey {
			return true
		}
	}
	return false
}

func (a *TransactionApprover) ResolveChainSymbol(coinKey string) string {
	if coinKey == "" || a.registry == nil {
		return "UNKNOWN"
	}
	cc, ok := a.registry.GetCoinChainBySafeheronKey(coinKey)
	if !ok || cc.Chain == nil {
		log.Printf("[approval] chain_symbol lookup failed for coinKey=%q, using UNKNOWN", coinKey)
		return "UNKNOWN"
	}
	return cc.Chain.ShortName
}
