package cliproxy

import (
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestAffinityMoveRoutingState(t *testing.T) {
	off, threshold := false, 75.0
	for _, tc := range []struct {
		name          string
		routing       internalconfig.RoutingConfig
		wantMove      bool
		wantThreshold float64
		wantMargin    time.Duration
	}{
		{name: "reset-first with affinity defaults on",
			routing:  internalconfig.RoutingConfig{Strategy: "reset-first", SessionAffinity: true},
			wantMove: true, wantThreshold: coreauth.DefaultAffinityMoveThreshold, wantMargin: coreauth.DefaultAffinityMoveMargin},
		{name: "explicit values",
			routing: internalconfig.RoutingConfig{Strategy: "reset-first", SessionAffinity: true,
				SessionAffinityMoveThreshold: &threshold, SessionAffinityMoveMargin: "12h"},
			wantMove: true, wantThreshold: 75, wantMargin: 12 * time.Hour},
		{name: "switched off",
			routing:  internalconfig.RoutingConfig{Strategy: "reset-first", SessionAffinity: true, SessionAffinityMove: &off},
			wantMove: false, wantThreshold: coreauth.DefaultAffinityMoveThreshold, wantMargin: coreauth.DefaultAffinityMoveMargin},
		{name: "fill-first never moves",
			routing:  internalconfig.RoutingConfig{Strategy: "fill-first", SessionAffinity: true},
			wantMove: false, wantThreshold: coreauth.DefaultAffinityMoveThreshold, wantMargin: coreauth.DefaultAffinityMoveMargin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := normalizedRoutingRuntimeState(&internalconfig.Config{Routing: tc.routing})
			if state.affinityMove != tc.wantMove || state.affinityMoveThreshold != tc.wantThreshold || state.affinityMoveMargin != tc.wantMargin {
				t.Fatalf("state = move %v threshold %v margin %v, want %v %v %v",
					state.affinityMove, state.affinityMoveThreshold, state.affinityMoveMargin, tc.wantMove, tc.wantThreshold, tc.wantMargin)
			}
			if _, ok := newRoutingSelector(state).(*coreauth.SessionAffinitySelector); !ok {
				t.Fatalf("selector type = %T, want *auth.SessionAffinitySelector", newRoutingSelector(state))
			}
		})
	}
}
