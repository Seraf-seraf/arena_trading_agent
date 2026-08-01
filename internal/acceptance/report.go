// Package acceptance builds a bounded, machine-readable v1 acceptance report
// from the durable controller journal.
package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
	"github.com/arena-trading-agent/arena-trading-agent/internal/repository"
	"github.com/arena-trading-agent/arena-trading-agent/internal/trading"
)

const (
	// CurrentSchemaVersion changes whenever the machine-readable evidence
	// contract gains or changes acceptance gates.
	CurrentSchemaVersion = 2

	defaultRequiredRuns = 50
	maxRequiredRuns     = 1000
	pageSize            = 500
	maxActionsPerRun    = 10_000
	maxEventsPerRun     = 10_000
	maxReasonsPerRun    = 100

	checkpointKind = "trading_checkpoint"
)

// Options selects the latest consecutive execution snapshots to inspect.
type Options struct {
	Runs          int
	OpportunityID string
	Since         time.Time
}

// Report is the stable JSON document emitted by the acceptance command.
type Report struct {
	SchemaVersion int        `json:"schema_version"`
	GeneratedAt   time.Time  `json:"generated_at"`
	Accepted      bool       `json:"accepted"`
	RequiredRuns  int        `json:"required_runs"`
	SelectedRuns  int        `json:"selected_runs"`
	OpportunityID string     `json:"opportunity_id,omitempty"`
	Since         *time.Time `json:"since,omitempty"`
	Reasons       []string   `json:"reasons,omitempty"`
	Criteria      Criteria   `json:"criteria"`
	Runs          []Run      `json:"runs"`
}

// Criteria contains aggregate counters and the exact boolean v1 gates.
type Criteria struct {
	CompletedExecutions         int  `json:"completed_executions"`
	ExecutionFailures           int  `json:"execution_failures"`
	StrictlyCompletedSagas      int  `json:"strictly_completed_sagas"`
	MatchedReconciliations      int  `json:"matched_reconciliations"`
	ErroneousMonetaryActions    int  `json:"erroneous_monetary_actions"`
	LostActions                 int  `json:"lost_actions"`
	ReconciliationFailures      int  `json:"reconciliation_failures"`
	ReconciliationMismatchItems int  `json:"reconciliation_mismatch_items"`
	InvalidBasedOnFrames        int  `json:"invalid_based_on_frames"`
	InvalidResultFrames         int  `json:"invalid_result_frames"`
	MissingMonetaryKinds        int  `json:"missing_monetary_kinds"`
	UnboundMonetaryActions      int  `json:"unbound_monetary_actions"`
	InvalidRouteSequences       int  `json:"invalid_route_sequences"`
	RecoveryAttempts            int  `json:"recovery_attempts"`
	SuccessfulRecoveries        int  `json:"successful_recoveries"`
	EmergencyEvents             int  `json:"emergency_events"`
	CriticalEvents              int  `json:"critical_events"`
	OverflowedRuns              int  `json:"overflowed_runs"`
	ZeroErroneousMoneyActions   bool `json:"zero_erroneous_monetary_actions"`
	ZeroLostActions             bool `json:"zero_lost_actions"`
	ZeroReconciliationMismatch  bool `json:"zero_reconciliation_mismatch"`
	AllActionsBoundToFrames     bool `json:"all_actions_bound_to_frames"`
	AllMonetaryKindsPresent     bool `json:"all_monetary_kinds_present"`
	AllMoneyBoundToCheckpoints  bool `json:"all_money_bound_to_checkpoints"`
	AllFixedRoutesValid         bool `json:"all_fixed_routes_valid"`
	RecoverySuccessRate100      bool `json:"recovery_success_rate_100"`
	NoEmergencyOrCriticalEvents bool `json:"no_emergency_or_critical_events"`
}

// Run is the evidence and rejection explanation for one execution snapshot.
type Run struct {
	ExecutionID      string                      `json:"execution_id"`
	OpportunityID    string                      `json:"opportunity_id"`
	StartedAt        time.Time                   `json:"started_at"`
	UpdatedAt        time.Time                   `json:"updated_at"`
	ExecutionStatus  domain.TradeExecutionStatus `json:"execution_status"`
	ExecutionFailure string                      `json:"execution_failure,omitempty"`
	SagaStatus       trading.SagaStatus          `json:"saga_status,omitempty"`
	SagaFailure      string                      `json:"saga_failure,omitempty"`
	Accepted         bool                        `json:"accepted"`
	Reasons          []string                    `json:"reasons,omitempty"`
	OmittedReasons   int                         `json:"omitted_reasons,omitempty"`

	CheckpointPresent           bool `json:"checkpoint_present"`
	ReconciliationExists        bool `json:"reconciliation_exists"`
	ReconciliationMatched       bool `json:"reconciliation_matched"`
	ReconciliationMismatchItems int  `json:"reconciliation_mismatch_items"`

	ActionRequests       int `json:"action_requests"`
	ActionResults        int `json:"action_results"`
	ExpectedMoneyActions int `json:"expected_monetary_actions"`
	LostActions          int `json:"lost_actions"`
	InvalidBasedOnFrames int `json:"invalid_based_on_frames"`
	InvalidResultFrames  int `json:"invalid_result_frames"`

	MonetaryActions          map[string]MonetaryActions `json:"monetary_actions"`
	ErroneousMonetaryActions int                        `json:"erroneous_monetary_actions"`
	UnboundMonetaryActions   int                        `json:"unbound_monetary_actions"`
	MissingMonetaryKinds     []string                   `json:"missing_monetary_kinds,omitempty"`
	FixedRouteValid          bool                       `json:"fixed_route_valid"`
	RecoveryAttempts         int                        `json:"recovery_attempts"`
	SuccessfulRecoveries     int                        `json:"successful_recoveries"`

	AgentEvents     int  `json:"agent_events"`
	EmergencyEvents int  `json:"emergency_events"`
	CriticalEvents  int  `json:"critical_events"`
	ActionOverflow  bool `json:"action_overflow"`
	EventOverflow   bool `json:"event_overflow"`
}

// MonetaryActions summarizes requests of one semantic money-action class.
type MonetaryActions struct {
	Requests   int `json:"requests"`
	Successful int `json:"successful"`
	Failed     int `json:"failed"`
}

type expectedMonetaryAction struct {
	ID         string
	Class      string
	StepIndex  int
	Order      int
	OccurredAt time.Time
}

type checkpointEvidence struct {
	actions []expectedMonetaryAction
}

// Generate inspects exactly the latest N matching current execution snapshots.
// Acceptance defects are returned in Report; only invalid input or repository
// access failures are returned as Go errors.
func Generate(ctx context.Context, store repository.Store, options Options) (Report, error) {
	const methodCtx = "acceptance.Generate"

	report := newReport(options)
	if ctx == nil {
		return report, fmt.Errorf("%s: контекст не задан", methodCtx)
	}
	if err := ctx.Err(); err != nil {
		return report, fmt.Errorf("%s: контекст завершён: %w", methodCtx, err)
	}
	if store == nil {
		return report, fmt.Errorf("%s: репозиторий не задан", methodCtx)
	}
	if options.Runs < 0 {
		return report, fmt.Errorf("%s: число прогонов не может быть отрицательным", methodCtx)
	}
	if report.RequiredRuns > maxRequiredRuns {
		return report, fmt.Errorf(
			"%s: запрошено %d прогонов, допустимо не более %d",
			methodCtx,
			report.RequiredRuns,
			maxRequiredRuns,
		)
	}
	if options.Since.Location() != time.UTC && !options.Since.IsZero() {
		options.Since = options.Since.UTC()
	}

	executions, err := store.ListExecutions(ctx, domain.ExecutionFilter{
		Since: options.Since,
		Limit: report.RequiredRuns,
	})
	if err != nil {
		return report, fmt.Errorf(
			"%s: не удалось получить текущие снимки исполнений: %w",
			methodCtx,
			err,
		)
	}
	report.SelectedRuns = len(executions)
	if len(executions) < report.RequiredRuns {
		report.Reasons = append(report.Reasons, fmt.Sprintf(
			"%s: найдено %d подходящих прогонов, требуется %d",
			methodCtx,
			len(executions),
			report.RequiredRuns,
		))
	}

	// Repository returns newest first; the report is chronological so the
	// asserted consecutive sequence is easy to inspect by machines and people.
	for left, right := 0, len(executions)-1; left < right; left, right = left+1, right-1 {
		executions[left], executions[right] = executions[right], executions[left]
	}
	report.Runs = make([]Run, 0, len(executions))
	fixedOpportunityID := report.OpportunityID
	if fixedOpportunityID == "" && len(executions) > 0 {
		fixedOpportunityID = strings.TrimSpace(executions[0].OpportunityID)
		report.OpportunityID = fixedOpportunityID
	}
	for _, execution := range executions {
		run, err := inspectRun(ctx, store, execution)
		if err != nil {
			return report, fmt.Errorf(
				"%s: не удалось проверить прогон %q: %w",
				methodCtx,
				execution.ID,
				err,
			)
		}
		if fixedOpportunityID == "" ||
			strings.TrimSpace(execution.OpportunityID) != fixedOpportunityID {
			run.addReason(fmt.Sprintf(
				"%s: прогон использует возможность %q вместо фиксированной %q",
				methodCtx,
				execution.OpportunityID,
				fixedOpportunityID,
			))
			run.FixedRouteValid = false
			run.Accepted = false
		}
		report.Runs = append(report.Runs, run)
		accumulate(&report.Criteria, run)
	}
	finalize(&report)
	return report, nil
}

func newReport(options Options) Report {
	requiredRuns := options.Runs
	if requiredRuns == 0 {
		requiredRuns = defaultRequiredRuns
	}
	report := Report{
		SchemaVersion: CurrentSchemaVersion,
		GeneratedAt:   time.Now().UTC(),
		RequiredRuns:  requiredRuns,
		OpportunityID: strings.TrimSpace(options.OpportunityID),
		Runs:          make([]Run, 0),
	}
	if !options.Since.IsZero() {
		since := options.Since.UTC()
		report.Since = &since
	}
	return report
}

func inspectRun(
	ctx context.Context,
	store repository.Store,
	execution domain.TradeExecution,
) (Run, error) {
	const methodCtx = "acceptance.inspectRun"

	run := Run{
		ExecutionID:      execution.ID,
		OpportunityID:    execution.OpportunityID,
		StartedAt:        execution.StartedAt,
		UpdatedAt:        execution.UpdatedAt,
		ExecutionStatus:  execution.Status,
		ExecutionFailure: execution.Failure,
		MonetaryActions:  emptyMonetaryActions(),
	}
	if execution.Status != domain.TradeCompleted {
		run.addReason(fmt.Sprintf(
			"%s: исполнение имеет статус %q вместо %q",
			methodCtx,
			execution.Status,
			domain.TradeCompleted,
		))
	}
	if strings.TrimSpace(execution.Failure) != "" {
		run.addReason(fmt.Sprintf(
			"%s: исполнение содержит ошибку: %s",
			methodCtx,
			execution.Failure,
		))
	}
	validInterval := true
	if execution.StartedAt.IsZero() {
		validInterval = false
		run.addReason(methodCtx + ": время начала исполнения не задано")
	}
	if execution.UpdatedAt.IsZero() {
		validInterval = false
		run.addReason(methodCtx + ": время завершения исполнения не задано")
	}
	if validInterval && execution.UpdatedAt.Before(execution.StartedAt) {
		validInterval = false
		run.addReason(methodCtx + ": время завершения предшествует времени начала")
	}

	evidence, err := inspectCheckpoint(ctx, store, execution, &run)
	if err != nil {
		return run, fmt.Errorf("%s: проверка контрольной точки завершилась ошибкой: %w", methodCtx, err)
	}
	if validInterval {
		if err := inspectActions(ctx, store, execution, evidence, &run); err != nil {
			return run, fmt.Errorf("%s: проверка действий завершилась ошибкой: %w", methodCtx, err)
		}
		if err := inspectEvents(ctx, store, execution, &run); err != nil {
			return run, fmt.Errorf("%s: проверка событий завершилась ошибкой: %w", methodCtx, err)
		}
	} else {
		run.addReason(methodCtx + ": журнал действий и событий нельзя связать с некорректным интервалом")
	}
	run.ExpectedMoneyActions = len(evidence.actions)
	for _, class := range monetaryClasses() {
		if run.MonetaryActions[class].Requests == 0 {
			run.MissingMonetaryKinds = append(run.MissingMonetaryKinds, class)
			run.addReason(fmt.Sprintf(
				"%s: в прогоне отсутствует обязательное денежное действие класса %s",
				methodCtx,
				class,
			))
		}
	}
	run.Accepted = len(run.Reasons) == 0 && run.OmittedReasons == 0
	return run, nil
}

func inspectCheckpoint(
	ctx context.Context,
	store repository.Store,
	execution domain.TradeExecution,
	run *Run,
) (checkpointEvidence, error) {
	const methodCtx = "acceptance.inspectCheckpoint"

	evidence := checkpointEvidence{actions: make([]expectedMonetaryAction, 0)}
	record, err := store.RuntimeRecord(ctx, "saga/"+execution.ID)
	if errors.Is(err, repository.ErrNotFound) {
		run.addReason(fmt.Sprintf(
			"%s: контрольная точка saga/%s не найдена",
			methodCtx,
			execution.ID,
		))
		return evidence, nil
	}
	if err != nil {
		return evidence, fmt.Errorf(
			"%s: не удалось прочитать контрольную точку saga/%s: %w",
			methodCtx,
			execution.ID,
			err,
		)
	}
	run.CheckpointPresent = true
	if record.Kind != checkpointKind {
		run.addReason(fmt.Sprintf(
			"%s: контрольная точка имеет тип %q вместо %q",
			methodCtx,
			record.Kind,
			checkpointKind,
		))
	}
	var checkpoint trading.Checkpoint
	if err := json.Unmarshal(record.Payload, &checkpoint); err != nil {
		run.addReason(fmt.Sprintf(
			"%s: контрольная точка не декодируется как trading.Checkpoint: %v",
			methodCtx,
			err,
		))
		return evidence, nil
	}
	_, checkpointValidationErr := trading.MonetaryActionIDsWithMaxStepAttempts(checkpoint, 3)
	if checkpointValidationErr != nil {
		run.addReason(fmt.Sprintf(
			"%s: контрольная точка не прошла строгую проверку: %v",
			methodCtx,
			checkpointValidationErr,
		))
	}
	run.SagaStatus = checkpoint.Saga.Status
	run.SagaFailure = checkpoint.Saga.Failure
	if checkpoint.Saga.ID != execution.ID {
		run.addReason(fmt.Sprintf(
			"%s: идентификатор саги %q не совпадает с исполнением %q",
			methodCtx,
			checkpoint.Saga.ID,
			execution.ID,
		))
	}
	if checkpoint.Saga.Opportunity.ID != execution.OpportunityID {
		run.addReason(fmt.Sprintf(
			"%s: возможность саги %q не совпадает с возможностью исполнения %q",
			methodCtx,
			checkpoint.Saga.Opportunity.ID,
			execution.OpportunityID,
		))
	}
	if checkpoint.Saga.Status != trading.SagaCompleted {
		run.addReason(fmt.Sprintf(
			"%s: сага имеет статус %q вместо строгого %q",
			methodCtx,
			checkpoint.Saga.Status,
			trading.SagaCompleted,
		))
	}
	if strings.TrimSpace(checkpoint.Saga.Failure) != "" {
		run.addReason(fmt.Sprintf(
			"%s: сага содержит ошибку: %s",
			methodCtx,
			checkpoint.Saga.Failure,
		))
	}
	reconciliation := checkpoint.Saga.Reconciliation
	if reconciliation == nil {
		run.addReason(methodCtx + ": результат сверки отсутствует")
		return evidence, nil
	}
	run.ReconciliationExists = true
	run.ReconciliationMatched = reconciliation.Matched
	run.ReconciliationMismatchItems = len(reconciliation.Mismatches)
	if reconciliation.ExecutionID != execution.ID {
		run.addReason(fmt.Sprintf(
			"%s: сверка относится к исполнению %q вместо %q",
			methodCtx,
			reconciliation.ExecutionID,
			execution.ID,
		))
	}
	if reconciliation.OpportunityID != execution.OpportunityID {
		run.addReason(fmt.Sprintf(
			"%s: сверка относится к возможности %q вместо %q",
			methodCtx,
			reconciliation.OpportunityID,
			execution.OpportunityID,
		))
	}
	if !reconciliation.Matched {
		run.addReason(methodCtx + ": сверка не подтверждает точное совпадение")
	}
	if len(reconciliation.Mismatches) != 0 {
		run.addReason(fmt.Sprintf(
			"%s: сверка содержит %d расхождений",
			methodCtx,
			len(reconciliation.Mismatches),
		))
	}
	if checkpointValidationErr != nil {
		return evidence, nil
	}
	evidence.actions = inspectFixedRoute(checkpoint, run)
	return evidence, nil
}

func inspectFixedRoute(
	checkpoint trading.Checkpoint,
	run *Run,
) []expectedMonetaryAction {
	actions := make([]expectedMonetaryAction, 0)
	valid := true
	reject := func(message string, arguments ...any) {
		const methodCtx = "acceptance.inspectFixedRoute.reject"

		valid = false
		run.addReason(fmt.Sprintf(methodCtx+": "+message, arguments...))
	}
	opportunity := checkpoint.Saga.Opportunity
	if opportunity.Kind != domain.OpportunityContactBarter {
		reject(
			"тип возможности %q не является фиксированным CONTACT_BARTER",
			opportunity.Kind,
		)
	}
	if !validFixedPlan(opportunity.Steps) {
		reject(
			"план должен содержать один или несколько BUY, затем ровно один BARTER и завершающий LIST",
		)
	}
	if checkpoint.Saga.CurrentStep != len(opportunity.Steps) {
		reject(
			"завершённая сага находится на шаге %d из %d",
			checkpoint.Saga.CurrentStep,
			len(opportunity.Steps),
		)
	}

	currentStep := 0
	awaitingRecovery := false
	saleSettled := false
	var previousOccurredAt time.Time
	for index, processed := range checkpoint.Processed {
		event := processed.Event
		if event.OccurredAt.IsZero() {
			reject("событие %q не содержит occurred_at", event.ID)
		} else {
			if !previousOccurredAt.IsZero() && event.OccurredAt.Before(previousOccurredAt) {
				reject("событие %q нарушает хронологический порядок", event.ID)
			}
			if event.OccurredAt.Before(run.StartedAt) || event.OccurredAt.After(run.UpdatedAt) {
				reject("событие %q находится вне интервала исполнения", event.ID)
			}
			previousOccurredAt = event.OccurredAt
		}
		if saleSettled {
			reject("после SALE_SETTLED найдено событие %q", event.ID)
		}
		switch event.Kind {
		case trading.EventStepSucceeded:
			if awaitingRecovery {
				reject("шаг %d продолжен без подтверждённого восстановления", event.StepIndex)
			}
			if event.StepIndex != currentStep ||
				event.StepIndex < 0 ||
				event.StepIndex >= len(opportunity.Steps) {
				reject(
					"успешное событие %q относится к шагу %d, ожидался %d",
					event.ID,
					event.StepIndex,
					currentStep,
				)
				continue
			}
			class, ok := monetaryClassForStep(opportunity.Steps[event.StepIndex])
			if !ok {
				reject(
					"шаг %d имеет неподдерживаемый тип %q",
					event.StepIndex,
					opportunity.Steps[event.StepIndex].Kind,
				)
				continue
			}
			if actionID := strings.TrimSpace(event.Outcome.MonetaryActionID); actionID != "" {
				actions = append(actions, expectedMonetaryAction{
					ID: actionID, Class: class, StepIndex: event.StepIndex,
					Order: len(actions), OccurredAt: event.OccurredAt,
				})
			}
			currentStep++
		case trading.EventStepFailed:
			run.RecoveryAttempts++
			if awaitingRecovery {
				reject("получен повторный сбой до завершения предыдущего восстановления")
			}
			if event.StepIndex != currentStep ||
				event.StepIndex < 0 ||
				event.StepIndex >= len(opportunity.Steps) {
				reject(
					"сбойное событие %q относится к шагу %d, ожидался %d",
					event.ID,
					event.StepIndex,
					currentStep,
				)
				awaitingRecovery = true
				continue
			}
			if actionID := strings.TrimSpace(event.Outcome.MonetaryActionID); actionID != "" {
				class, ok := monetaryClassForStep(opportunity.Steps[event.StepIndex])
				if !ok {
					reject(
						"сбойный шаг %d имеет неподдерживаемый тип %q",
						event.StepIndex,
						opportunity.Steps[event.StepIndex].Kind,
					)
				} else {
					actions = append(actions, expectedMonetaryAction{
						ID: actionID, Class: class, StepIndex: event.StepIndex,
						Order: len(actions), OccurredAt: event.OccurredAt,
					})
				}
			}
			awaitingRecovery = true
		case trading.EventRecoveryResolved:
			if !awaitingRecovery {
				reject("событие восстановления %q не имеет предшествующего сбоя", event.ID)
			}
			if event.Resolution != trading.RecoveryRetry {
				reject(
					"фиксированный успешный маршрут содержит терминальное восстановление %q",
					event.Resolution,
				)
			} else if awaitingRecovery {
				run.SuccessfulRecoveries++
			}
			awaitingRecovery = false
		case trading.EventSaleSettled:
			if awaitingRecovery {
				reject("продажа сверена при незавершённом восстановлении")
			}
			if currentStep != len(opportunity.Steps) {
				reject(
					"продажа сверена после шага %d из %d",
					currentStep,
					len(opportunity.Steps),
				)
			}
			if saleSettled {
				reject("событие SALE_SETTLED продублировано")
			}
			if event.Settlement == nil ||
				event.Settlement.ExecutionID != checkpoint.Saga.ID ||
				strings.TrimSpace(event.Settlement.MarketOrderID) == "" {
				reject("событие SALE_SETTLED не содержит точную сверку ордера")
			}
			saleSettled = true
		default:
			reject("событие %q имеет неизвестный тип %q", event.ID, event.Kind)
		}
		if index == len(checkpoint.Processed)-1 &&
			event.Kind != trading.EventSaleSettled {
			reject("последнее событие фиксированного маршрута не является SALE_SETTLED")
		}
	}
	if currentStep != len(opportunity.Steps) {
		reject("подтверждено %d из %d шагов плана", currentStep, len(opportunity.Steps))
	}
	if !saleSettled {
		reject("событие SALE_SETTLED отсутствует")
	}
	if awaitingRecovery || run.RecoveryAttempts != run.SuccessfulRecoveries {
		reject(
			"успешно завершено %d из %d восстановлений",
			run.SuccessfulRecoveries,
			run.RecoveryAttempts,
		)
	}
	run.FixedRouteValid = valid
	return actions
}

func validFixedPlan(steps []domain.TradeStep) bool {
	if len(steps) < 3 {
		return false
	}
	index := 0
	for index < len(steps) && steps[index].Kind == domain.TradeStepBuy {
		if strings.TrimSpace(steps[index].ItemID) == "" || steps[index].Quantity <= 0 {
			return false
		}
		index++
	}
	if index == 0 || index >= len(steps) ||
		steps[index].Kind != domain.TradeStepBarter ||
		strings.TrimSpace(steps[index].RecipeID) == "" ||
		strings.TrimSpace(steps[index].ItemID) == "" ||
		steps[index].Quantity <= 0 {
		return false
	}
	resultItemID := strings.TrimSpace(steps[index].ItemID)
	index++
	if index != len(steps)-1 ||
		steps[index].Kind != domain.TradeStepList ||
		strings.TrimSpace(steps[index].ItemID) != resultItemID ||
		steps[index].Quantity <= 0 {
		return false
	}
	return true
}

func monetaryClassForStep(step domain.TradeStep) (string, bool) {
	switch step.Kind {
	case domain.TradeStepBuy:
		return string(protocol.ActionPurchase), true
	case domain.TradeStepBarter:
		return string(protocol.ActionBarter), true
	case domain.TradeStepList:
		return string(protocol.ActionListing), true
	default:
		return "", false
	}
}

func inspectActions(
	ctx context.Context,
	store repository.Store,
	execution domain.TradeExecution,
	evidence checkpointEvidence,
	run *Run,
) error {
	const methodCtx = "acceptance.inspectActions"

	expected := make(map[string]expectedMonetaryAction, len(evidence.actions))
	for _, item := range evidence.actions {
		expected[item.ID] = item
	}
	seen := make(map[string]struct{}, len(expected))
	orderTimes := make([]time.Time, len(evidence.actions))
	offset := 0
	for {
		limit := pageSize
		if remaining := maxActionsPerRun - run.ActionRequests; remaining < limit {
			limit = remaining
		}
		if limit == 0 {
			extra, err := store.ListActions(ctx, domain.ActionFilter{
				Since:  execution.StartedAt,
				Until:  execution.UpdatedAt,
				Limit:  1,
				Offset: offset,
			})
			if err != nil {
				return fmt.Errorf("%s: не удалось проверить переполнение журнала действий: %w", methodCtx, err)
			}
			if len(extra) > 0 {
				run.ActionOverflow = true
				run.addReason(fmt.Sprintf(
					"%s: число действий превышает безопасный лимит %d",
					methodCtx,
					maxActionsPerRun,
				))
			}
			return nil
		}
		actions, err := store.ListActions(ctx, domain.ActionFilter{
			Since:  execution.StartedAt,
			Until:  execution.UpdatedAt,
			Limit:  limit,
			Offset: offset,
		})
		if err != nil {
			return fmt.Errorf("%s: не удалось получить страницу действий: %w", methodCtx, err)
		}
		if len(actions) == 0 {
			return nil
		}
		for _, action := range actions {
			run.ActionRequests++
			inspectActionBasis(action, run)
			result, err := store.ActionResult(ctx, action.ID)
			if errors.Is(err, repository.ErrNotFound) {
				run.LostActions++
				run.addReason(fmt.Sprintf(
					"%s: для действия %q отсутствует ActionResult",
					methodCtx,
					action.ID,
				))
				inspectMonetaryAction(
					action,
					nil,
					expected,
					seen,
					orderTimes,
					run,
				)
				continue
			}
			if err != nil {
				return fmt.Errorf(
					"%s: не удалось прочитать результат действия %q: %w",
					methodCtx,
					action.ID,
					err,
				)
			}
			run.ActionResults++
			inspectActionResult(action, result, run)
			inspectMonetaryAction(
				action,
				&result,
				expected,
				seen,
				orderTimes,
				run,
			)
		}
		offset += len(actions)
		if len(actions) < limit {
			break
		}
	}
	for _, item := range evidence.actions {
		if _, ok := seen[item.ID]; ok {
			continue
		}
		action, err := store.Action(ctx, item.ID)
		if errors.Is(err, repository.ErrNotFound) {
			run.LostActions++
			run.addReason(fmt.Sprintf(
				"%s: контрольная точка ссылается на отсутствующее действие %q",
				methodCtx,
				item.ID,
			))
			continue
		}
		if err != nil {
			return fmt.Errorf(
				"%s: не удалось прочитать действие %q из контрольной точки: %w",
				methodCtx,
				item.ID,
				err,
			)
		}
		run.addReason(fmt.Sprintf(
			"%s: точное действие %q находится вне интервала исполнения",
			methodCtx,
			item.ID,
		))
		inspectActionBasis(action, run)
		result, resultErr := store.ActionResult(ctx, action.ID)
		if errors.Is(resultErr, repository.ErrNotFound) {
			run.LostActions++
			run.addReason(fmt.Sprintf(
				"%s: для точного действия %q отсутствует ActionResult",
				methodCtx,
				action.ID,
			))
			inspectMonetaryAction(
				action,
				nil,
				expected,
				seen,
				orderTimes,
				run,
			)
			continue
		}
		if resultErr != nil {
			return fmt.Errorf(
				"%s: не удалось прочитать результат точного действия %q: %w",
				methodCtx,
				action.ID,
				resultErr,
			)
		}
		inspectActionResult(action, result, run)
		inspectMonetaryAction(
			action,
			&result,
			expected,
			seen,
			orderTimes,
			run,
		)
	}
	for index := 1; index < len(orderTimes); index++ {
		if orderTimes[index-1].IsZero() || orderTimes[index].IsZero() {
			continue
		}
		if orderTimes[index].Before(orderTimes[index-1]) {
			run.FixedRouteValid = false
			run.addReason(fmt.Sprintf(
				"%s: денежные действия %q и %q нарушают порядок торгового плана",
				methodCtx,
				evidence.actions[index-1].ID,
				evidence.actions[index].ID,
			))
		}
	}
	return nil
}

func inspectMonetaryAction(
	action domain.ActionRecord,
	result *domain.ActionResultRecord,
	expected map[string]expectedMonetaryAction,
	seen map[string]struct{},
	orderTimes []time.Time,
	run *Run,
) {
	const methodCtx = "acceptance.inspectMonetaryAction"

	class := action.Class
	_, monetary := run.MonetaryActions[class]
	item, isExpected := expected[action.ID]
	if !monetary && !isExpected {
		return
	}
	if !isExpected {
		if result != nil && result.NotSent && result.RetrySafe && !result.Success {
			return
		}
		run.UnboundMonetaryActions++
		run.ErroneousMonetaryActions++
		run.addReason(fmt.Sprintf(
			"%s: денежное действие %q класса %s не связано с контрольной точкой",
			methodCtx,
			action.ID,
			class,
		))
		return
	}
	if _, duplicate := seen[action.ID]; duplicate {
		run.ErroneousMonetaryActions++
		run.addReason(fmt.Sprintf(
			"%s: точное денежное действие %q найдено повторно",
			methodCtx,
			action.ID,
		))
		return
	}
	seen[action.ID] = struct{}{}
	if item.Order >= 0 && item.Order < len(orderTimes) {
		orderTimes[item.Order] = action.RequestedAt
	}
	summary := run.MonetaryActions[item.Class]
	summary.Requests++
	if class != item.Class {
		summary.Failed++
		run.ErroneousMonetaryActions++
		run.addReason(fmt.Sprintf(
			"%s: действие %q имеет класс %s вместо %s для шага %d",
			methodCtx,
			action.ID,
			class,
			item.Class,
			item.StepIndex,
		))
		run.MonetaryActions[item.Class] = summary
		return
	}
	if !item.OccurredAt.IsZero() &&
		(action.RequestedAt.IsZero() || action.RequestedAt.After(item.OccurredAt)) {
		summary.Failed++
		run.ErroneousMonetaryActions++
		run.addReason(fmt.Sprintf(
			"%s: действие %q запрошено позже связанного торгового события",
			methodCtx,
			action.ID,
		))
		run.MonetaryActions[item.Class] = summary
		return
	}
	if result != nil &&
		result.Success &&
		!result.NotSent &&
		strings.TrimSpace(result.Error) == "" &&
		(item.OccurredAt.IsZero() ||
			(!result.CompletedAt.IsZero() && !result.CompletedAt.After(item.OccurredAt))) {
		summary.Successful++
	} else {
		summary.Failed++
		run.ErroneousMonetaryActions++
		run.addReason(fmt.Sprintf(
			"%s: денежное действие %q класса %s не подтверждено как успешное",
			methodCtx,
			action.ID,
			class,
		))
	}
	run.MonetaryActions[item.Class] = summary
}

func inspectActionBasis(action domain.ActionRecord, run *Run) {
	const methodCtx = "acceptance.inspectActionBasis"

	frameBasisValid := true
	var frameBasis []protocol.FrameRegionDigest
	if len(action.FrameBasisPayload) > 0 {
		if err := json.Unmarshal(action.FrameBasisPayload, &frameBasis); err != nil ||
			protocol.ValidateFrameRegionBasis(frameBasis) != nil {
			frameBasisValid = false
		}
	}
	if isMonetaryClass(action.Class) && len(frameBasis) == 0 {
		frameBasisValid = false
	}
	valid := action.BasedOnFrame != 0 &&
		action.BasedOnCapturedAt != nil &&
		!action.BasedOnCapturedAt.IsZero() &&
		protocol.ValidFrameDigest(action.BasedOnFrameDigest) &&
		action.BasedOnState != "" &&
		action.BasedOnState != domain.StateUnknown &&
		action.ExpectedState != "" &&
		action.ExpectedState != domain.StateUnknown &&
		action.ExpectedWidth > 0 &&
		action.ExpectedHeight > 0 &&
		action.ExpectedDPIPercent > 0 &&
		!action.Deadline.IsZero() &&
		!action.RequestedAt.IsZero() &&
		action.Deadline.After(action.RequestedAt) &&
		frameBasisValid
	if valid {
		return
	}
	run.InvalidBasedOnFrames++
	run.addReason(fmt.Sprintf(
		"%s: действие %q не содержит полного проверяемого основания кадра, состояния, геометрии и срока",
		methodCtx,
		action.ID,
	))
}

func isMonetaryClass(class string) bool {
	switch protocol.ActionClass(class) {
	case protocol.ActionPurchase, protocol.ActionBarter,
		protocol.ActionListing, protocol.ActionReprice:
		return true
	default:
		return false
	}
}

func inspectActionResult(
	action domain.ActionRecord,
	result domain.ActionResultRecord,
	run *Run,
) {
	const methodCtx = "acceptance.inspectActionResult"

	if result.NotSent && result.RetrySafe && !result.Success {
		valid := result.ActionID == action.ID &&
			result.ResultFrame == action.BasedOnFrame &&
			!result.CompletedAt.IsZero() &&
			!result.ReceivedAt.IsZero() &&
			!result.ReceivedAt.Before(result.CompletedAt)
		if valid {
			return
		}
	}
	valid := !result.NotSent &&
		result.ActionID == action.ID &&
		result.ResultFrame > action.BasedOnFrame &&
		result.ResultState != "" &&
		result.ResultState != domain.StateUnknown &&
		!result.CompletedAt.IsZero() &&
		!result.ReceivedAt.IsZero() &&
		!result.ReceivedAt.Before(result.CompletedAt)
	if result.Success {
		valid = valid &&
			result.ResultState == action.ExpectedState &&
			result.VerificationConfidence > 0 &&
			result.VerificationConfidence <= 1 &&
			!result.CompletedAt.After(action.Deadline)
	}
	if valid {
		return
	}
	run.InvalidResultFrames++
	run.addReason(fmt.Sprintf(
		"%s: результат действия %q не содержит согласованного контрольного кадра, состояния или времени",
		methodCtx,
		action.ID,
	))
}

func inspectEvents(
	ctx context.Context,
	store repository.Store,
	execution domain.TradeExecution,
	run *Run,
) error {
	const methodCtx = "acceptance.inspectEvents"

	offset := 0
	for {
		limit := pageSize
		if remaining := maxEventsPerRun - run.AgentEvents; remaining < limit {
			limit = remaining
		}
		if limit == 0 {
			extra, err := store.ListEvents(ctx, domain.EventFilter{
				Since:  execution.StartedAt,
				Until:  execution.UpdatedAt,
				Limit:  1,
				Offset: offset,
			})
			if err != nil {
				return fmt.Errorf("%s: не удалось проверить переполнение журнала событий: %w", methodCtx, err)
			}
			if len(extra) > 0 {
				run.EventOverflow = true
				run.addReason(fmt.Sprintf(
					"%s: число событий превышает безопасный лимит %d",
					methodCtx,
					maxEventsPerRun,
				))
			}
			return nil
		}
		events, err := store.ListEvents(ctx, domain.EventFilter{
			Since:  execution.StartedAt,
			Until:  execution.UpdatedAt,
			Limit:  limit,
			Offset: offset,
		})
		if err != nil {
			return fmt.Errorf("%s: не удалось получить страницу событий: %w", methodCtx, err)
		}
		if len(events) == 0 {
			return nil
		}
		for _, event := range events {
			run.AgentEvents++
			if isEmergencyStop(event.Kind) {
				run.EmergencyEvents++
				run.addReason(fmt.Sprintf(
					"%s: в интервале найдено аварийное событие %q",
					methodCtx,
					event.Kind,
				))
			}
			if strings.EqualFold(strings.TrimSpace(event.Severity), "critical") {
				run.CriticalEvents++
				run.addReason(fmt.Sprintf(
					"%s: в интервале найдено критическое событие %q",
					methodCtx,
					event.Kind,
				))
			}
		}
		offset += len(events)
		if len(events) < limit {
			return nil
		}
	}
}

func isEmergencyStop(kind string) bool {
	normalized := strings.ToUpper(strings.TrimSpace(kind))
	return strings.Contains(normalized, "EMERGENCY_STOP")
}

func monetaryClasses() []string {
	return []string{
		string(protocol.ActionPurchase),
		string(protocol.ActionBarter),
		string(protocol.ActionListing),
	}
}

func emptyMonetaryActions() map[string]MonetaryActions {
	return map[string]MonetaryActions{
		string(protocol.ActionPurchase): {},
		string(protocol.ActionBarter):   {},
		string(protocol.ActionListing):  {},
	}
}

func (run *Run) addReason(reason string) {
	if len(run.Reasons) < maxReasonsPerRun {
		run.Reasons = append(run.Reasons, reason)
		return
	}
	run.OmittedReasons++
}

func accumulate(criteria *Criteria, run Run) {
	if run.ExecutionStatus == domain.TradeCompleted {
		criteria.CompletedExecutions++
	}
	if run.ExecutionStatus != domain.TradeCompleted ||
		strings.TrimSpace(run.ExecutionFailure) != "" {
		criteria.ExecutionFailures++
	}
	if run.SagaStatus == trading.SagaCompleted {
		criteria.StrictlyCompletedSagas++
	}
	if run.ReconciliationExists && run.ReconciliationMatched &&
		run.ReconciliationMismatchItems == 0 {
		criteria.MatchedReconciliations++
	} else {
		criteria.ReconciliationFailures++
	}
	criteria.ReconciliationMismatchItems += run.ReconciliationMismatchItems
	criteria.ErroneousMonetaryActions += run.ErroneousMonetaryActions
	criteria.LostActions += run.LostActions
	criteria.InvalidBasedOnFrames += run.InvalidBasedOnFrames
	criteria.InvalidResultFrames += run.InvalidResultFrames
	criteria.MissingMonetaryKinds += len(run.MissingMonetaryKinds)
	criteria.UnboundMonetaryActions += run.UnboundMonetaryActions
	if !run.FixedRouteValid {
		criteria.InvalidRouteSequences++
	}
	criteria.RecoveryAttempts += run.RecoveryAttempts
	criteria.SuccessfulRecoveries += run.SuccessfulRecoveries
	criteria.EmergencyEvents += run.EmergencyEvents
	criteria.CriticalEvents += run.CriticalEvents
	if run.ActionOverflow || run.EventOverflow {
		criteria.OverflowedRuns++
	}
}

func finalize(report *Report) {
	criteria := &report.Criteria
	criteria.ZeroErroneousMoneyActions = criteria.ErroneousMonetaryActions == 0
	criteria.ZeroLostActions = criteria.LostActions == 0
	criteria.ZeroReconciliationMismatch =
		criteria.ReconciliationFailures == 0 &&
			criteria.ReconciliationMismatchItems == 0
	criteria.AllActionsBoundToFrames =
		criteria.InvalidBasedOnFrames == 0 && criteria.InvalidResultFrames == 0
	criteria.AllMonetaryKindsPresent = criteria.MissingMonetaryKinds == 0
	criteria.AllMoneyBoundToCheckpoints = criteria.UnboundMonetaryActions == 0
	criteria.AllFixedRoutesValid = criteria.InvalidRouteSequences == 0
	criteria.RecoverySuccessRate100 =
		criteria.RecoveryAttempts == criteria.SuccessfulRecoveries
	criteria.NoEmergencyOrCriticalEvents =
		criteria.EmergencyEvents == 0 && criteria.CriticalEvents == 0

	allRunsAccepted := report.SelectedRuns == report.RequiredRuns
	for _, run := range report.Runs {
		if !run.Accepted {
			allRunsAccepted = false
			break
		}
	}
	report.Accepted = allRunsAccepted &&
		criteria.CompletedExecutions == report.RequiredRuns &&
		criteria.StrictlyCompletedSagas == report.RequiredRuns &&
		criteria.MatchedReconciliations == report.RequiredRuns &&
		criteria.ZeroErroneousMoneyActions &&
		criteria.ZeroLostActions &&
		criteria.ZeroReconciliationMismatch &&
		criteria.AllActionsBoundToFrames &&
		criteria.AllMonetaryKindsPresent &&
		criteria.AllMoneyBoundToCheckpoints &&
		criteria.AllFixedRoutesValid &&
		criteria.RecoverySuccessRate100 &&
		criteria.NoEmergencyOrCriticalEvents &&
		criteria.OverflowedRuns == 0
}
