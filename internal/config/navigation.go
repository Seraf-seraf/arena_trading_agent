package config

import (
	"fmt"

	"github.com/arena-trading-agent/arena-trading-agent/internal/navigation"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

// NavigationTransitions compiles validated declarative configuration into the
// runtime graph. Multiple configured inputs are sent as one locally
// serialized SEQUENCE and verified with one exact post-action frame.
func (r *Runtime) NavigationTransitions() ([]navigation.Transition, error) {
	const methodCtx = "config.Runtime.NavigationTransitions"

	if err := r.Validate(); err != nil {
		return nil, fmt.Errorf("%s: не удалось проверить конфигурацию: %w", methodCtx, err)
	}
	result := make([]navigation.Transition, 0, len(r.Navigation.Transitions))
	for _, configured := range r.Navigation.Transitions {
		steps := make([]protocol.Action, 0, len(configured.Actions))
		for _, action := range configured.Actions {
			steps = append(steps, action.Commands()...)
		}
		if len(steps) == 0 {
			return nil, fmt.Errorf("%s: переход %q не содержит исполняемых действий", methodCtx, configured.ID)
		}
		action := steps[0]
		if len(steps) > 1 {
			action = protocol.Action{Kind: "SEQUENCE", Steps: steps}
		}
		verify := navigation.VerificationRule{
			State:         configured.Verify.State,
			MinConfidence: configured.Verify.MinConfidence,
			Timeout:       configured.Verify.Timeout.Value(),
		}
		result = append(result, navigation.Transition{
			From:     configured.From,
			To:       configured.To,
			Action:   action,
			Class:    configured.Class,
			Verify:   verify,
			MaxRetry: configured.MaxRetry,
		})
	}
	return result, nil
}
