package approval

import (
	"context"

	"monera-digital/internal/safeheron"
)

type ApprovalDecision struct {
	Action       string // APPROVE / REJECT
	Reason       string
	AmlRiskLevel string // v1.1 Phase 1: AML 等级快照，非 AUTO_SWEEP/UTXO_COLLECTION 时为空
}

type Approver interface {
	Evaluate(ctx context.Context, bizContent *safeheron.CoSignerBizContentV3) (*ApprovalDecision, error)
}

type DefaultRejectApprover struct{}

func (a *DefaultRejectApprover) Evaluate(_ context.Context, biz *safeheron.CoSignerBizContentV3) (*ApprovalDecision, error) {
	return &ApprovalDecision{
		Action: "REJECT",
		Reason: "unsupported callback type: " + biz.Type,
	}, nil
}

type CallbackTestApprover struct{}

func (a *CallbackTestApprover) Evaluate(_ context.Context, _ *safeheron.CoSignerBizContentV3) (*ApprovalDecision, error) {
	return &ApprovalDecision{
		Action: "APPROVE",
		Reason: "CALLBACK_TEST",
	}, nil
}
