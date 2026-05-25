package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"monera-digital/internal/safeheron"
)

type AlertFunc func(level, title string, fields map[string]string)

type ApprovalService struct {
	repo       Repository
	txApprover *TransactionApprover
	alertFn    AlertFunc
}

func NewApprovalService(repo Repository, txApprover *TransactionApprover, alertFn AlertFunc) *ApprovalService {
	return &ApprovalService{
		repo:       repo,
		txApprover: txApprover,
		alertFn:    alertFn,
	}
}

func (s *ApprovalService) Evaluate(ctx context.Context, biz *safeheron.CoSignerBizContentV3) (*ApprovalDecision, error) {
	approver := s.selectApprover(biz.Type)
	decision, err := approver.Evaluate(ctx, biz)
	if err != nil {
		return nil, fmt.Errorf("approver evaluate: %w", err)
	}

	chainSymbol := "UNKNOWN"
	var detail safeheron.TransactionApproval
	hasDetail := false
	if biz.Type == "TRANSACTION" {
		if err := json.Unmarshal(biz.Detail, &detail); err != nil {
			log.Printf("[approval] WARN: failed to parse TRANSACTION detail approvalId=%s: %v", biz.ApprovalId, err)
		} else {
			hasDetail = true
		}
	}

	if hasDetail {
		chainSymbol = s.txApprover.ResolveChainSymbol(detail.CoinKey)
	}

	rawBytes, err := json.Marshal(biz)
	if err != nil {
		log.Printf("[approval] ERROR: failed to marshal biz for audit approvalId=%s: %v", biz.ApprovalId, err)
	}
	rec := &ApprovalRecord{
		ApprovalID:   biz.ApprovalId,
		CallbackType: biz.Type,
		Action:       decision.Action,
		Reason:       decision.Reason,
		ChainSymbol:  chainSymbol,
		RawRequest:   rawBytes,
		AmlRiskLevel: decision.AmlRiskLevel, // v1.1: AUTO_SWEEP/UTXO_COLLECTION 才有值，其他为 ""（落 NULL）
	}

	if hasDetail {
		rec.TxType = detail.TransactionType
		rec.TxKey = detail.TxKey
		rec.CoinKey = detail.CoinKey
		rec.TxAmount = detail.TxAmount
		rec.SourceAccountKey = detail.SourceAccountKey
		rec.DestinationAccountKey = detail.DestinationAccountKey
		rec.DestinationAccountType = detail.DestinationAccountType
		rec.DestinationAddress = detail.DestinationAddress
		rec.CustomerRefID = detail.CustomerRefId
	}

	if rec.RawRequest == nil {
		rec.RawRequest = json.RawMessage(`{}`)
	}

	if err := s.repo.InsertApprovalRecord(ctx, rec); err != nil {
		if errors.Is(err, ErrDuplicateApproval) {
			return s.handleIdempotent(ctx, biz.ApprovalId)
		}
		return nil, fmt.Errorf("insert approval record: %w", err)
	}

	if decision.Action == "APPROVE" && hasDetail {
		st := &SweepTransaction{
			TxKey:                 detail.TxKey,
			TxHash:                detail.TxHash,
			CustomerRefID:         detail.CustomerRefId,
			TxType:                detail.TransactionType,
			ChainSymbol:           chainSymbol,
			CoinKey:               detail.CoinKey,
			FeeCoinKey:            detail.FeeCoinKey,
			TxAmount:              detail.TxAmount,
			EstimateFee:           detail.EstimateFee,
			SourceAccountKey:      detail.SourceAccountKey,
			SourceAddress:         detail.SourceAddress,
			DestinationAccountKey: detail.DestinationAccountKey,
			DestinationAddress:    detail.DestinationAddress,
			TxStatus:              detail.TransactionStatus,
			TxSubStatus:           detail.TransactionSubStatus,
			ApprovalID:            biz.ApprovalId,
			ApprovalAction:        "APPROVE",
		}
		if st.TxStatus == "" {
			st.TxStatus = "PENDING"
		}
		if err := s.repo.InsertSweepTransaction(ctx, st); err != nil {
			if !errors.Is(err, ErrDuplicateSweepTx) {
				return nil, fmt.Errorf("insert sweep transaction: %w", err)
			}
		}
	}

	if decision.Action == "REJECT" {
		sourceAddr := ""
		if hasDetail {
			sourceAddr = detail.SourceAddress
		}
		s.sendRejectAlert(biz, decision, rec, sourceAddr)
	}

	return decision, nil
}

func (s *ApprovalService) selectApprover(callbackType string) Approver {
	switch callbackType {
	case "TRANSACTION":
		return s.txApprover
	case "CALLBACK_TEST":
		return &CallbackTestApprover{}
	default:
		return &DefaultRejectApprover{}
	}
}

func (s *ApprovalService) handleIdempotent(ctx context.Context, approvalID string) (*ApprovalDecision, error) {
	existing, err := s.repo.GetApprovalByID(ctx, approvalID)
	if err != nil {
		return nil, fmt.Errorf("idempotent lookup: %w", err)
	}
	log.Printf("[approval] idempotent hit for approvalId=%s, returning previous action=%s", approvalID, existing.Action)
	return &ApprovalDecision{
		Action:       existing.Action,
		Reason:       existing.Reason,
		AmlRiskLevel: existing.AmlRiskLevel,
	}, nil
}

func (s *ApprovalService) sendRejectAlert(biz *safeheron.CoSignerBizContentV3, decision *ApprovalDecision, rec *ApprovalRecord, sourceAddress string) {
	if s.alertFn == nil {
		log.Printf("[approval] WARN: alertFn is nil, REJECT alert suppressed approvalId=%s reason=%s", biz.ApprovalId, decision.Reason)
		return
	}
	fields := map[string]string{
		"approvalId": biz.ApprovalId,
		"type":       biz.Type,
		"reason":     decision.Reason,
		"action":     "REJECT",
	}
	if rec.TxKey != "" {
		fields["txKey"] = rec.TxKey
	}
	if rec.CoinKey != "" {
		fields["coinKey"] = rec.CoinKey
	}
	if rec.TxAmount != "" {
		fields["txAmount"] = rec.TxAmount
	}
	if rec.TxType != "" {
		fields["txType"] = rec.TxType
	}
	if rec.DestinationAddress != "" {
		fields["destinationAddress"] = rec.DestinationAddress
	}
	if sourceAddress != "" {
		fields["sourceAddress"] = sourceAddress
	}
	// v1.1 Phase 1 (D-AML-7)：AML 路径触发的 REJECT 一律 WARN（不分等级）；
	// 非 AML 路径（白名单失败 / 非 VAULT_ACCOUNT 等）保持 ERROR。
	level := "ERROR"
	if decision.AmlRiskLevel != "" {
		level = "WARN"
		fields["riskLevel"] = decision.AmlRiskLevel
	}
	s.alertFn(level, "审批回调 REJECT", fields)
}
