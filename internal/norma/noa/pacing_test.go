// Modified by ARTEX for in-tree integration; see ../NOTICE.
package noa

import "testing"

// Production defaults intentionally pin GrowthFloor and GrowthCap to the same
// tuned value. Adaptive pacing is supported for callers that configure a real
// clamp band, so test that input rather than turning unrelated default tuning
// into an accidental requirement. Driving DecideNudge also catches a consumer
// that accepts the configuration but computes cadence from the defaults or the
// wrong context window.
func TestConfiguredAdaptiveCadenceTracksWindowWithinClampBand(t *testing.T) {
	nudge := NudgeConfig{
		MaxContextLimitPct:    0.90,
		MinContextLimitPct:    0.45,
		EmergencyThresholdPct: 0.95,
		GrowthRatio:           0.10,
		GrowthFloor:           2_000,
		GrowthCap:             8_000,
		MinGrowthFloor:        1_000,
		MinGrowthRatio:        0.50,
		Tier2GrowthMultiplier: 1.5,
	}
	cases := []struct {
		name         string
		window       int
		tokenCount   int
		wantAdaptive int
		wantCadence  int
	}{
		{name: "growth floor", window: 10_000, tokenCount: 8_000, wantAdaptive: 2_000, wantCadence: 1_000},
		{name: "ratio", window: 40_000, tokenCount: 32_000, wantAdaptive: 4_000, wantCadence: 2_000},
		{name: "growth cap", window: 100_000, tokenCount: 80_000, wantAdaptive: 8_000, wantCadence: 4_000},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{ModelContextLimit: tc.window, Nudge: nudge}
			decideAfterGrowth := func(growth int) NudgeDecision {
				st := CreateInitialState("s", "")
				st.Nudge.LastNudgeShownTokens = tc.tokenCount - growth
				st.Nudge.LastPerMessageNudgeTokens = tc.tokenCount - growth
				return DecideNudge(DecideNudgeInput{
					State:      st,
					Config:     cfg,
					TokenCount: tc.tokenCount,
					Recommendation: Recommendation{CompressibleRanges: []RangeInfo{{
						StartRef: "m00002", EndRef: "m00050", Count: 20,
						Tokens: tc.wantAdaptive, Chars: tc.wantAdaptive * 4,
					}}},
				})
			}

			below := decideAfterGrowth(tc.wantCadence - 1)
			if below.Breakdown.AdaptiveGrowth != tc.wantAdaptive {
				t.Fatalf("adaptive growth = %d, want %d", below.Breakdown.AdaptiveGrowth, tc.wantAdaptive)
			}
			if below.Breakdown.GrowthFloor != tc.wantCadence {
				t.Fatalf("decision cadence = %d, want %d", below.Breakdown.GrowthFloor, tc.wantCadence)
			}
			if got := NudgeGrowthFloor(cfg); got != tc.wantCadence {
				t.Fatalf("host cadence = %d, want %d", got, tc.wantCadence)
			}
			if below.ShouldInject {
				t.Fatalf("injected after %d tokens of growth, below the %d-token cadence: %s",
					tc.wantCadence-1, tc.wantCadence, below.Reason)
			}

			atGate := decideAfterGrowth(tc.wantCadence)
			if !atGate.ShouldInject || atGate.Tier != 1 {
				t.Fatalf("decision at the configured cadence = inject:%v tier:%d, want a tier-1 nudge: %s",
					atGate.ShouldInject, atGate.Tier, atGate.Reason)
			}
		})
	}
}

// Suppression resumes after NudgeGrowthFloor, not after the larger adaptive
// backlog threshold. Exact configured cases cover both sides of MinGrowthFloor
// and the adaptive cap; they catch a host that ignores the minimum or waits for
// the wrong threshold. The resulting release point is checked against the
// configured pressure boundary rather than an arbitrary fraction of defaults.
func TestConfiguredSuppressionReleaseUsesCadenceFloor(t *testing.T) {
	nudge := NudgeConfig{
		MaxContextLimitPct: 0.75,
		GrowthRatio:        0.10,
		GrowthFloor:        2_000,
		GrowthCap:          8_000,
		MinGrowthFloor:     1_500,
		MinGrowthRatio:     0.25,
	}
	cases := []struct {
		name          string
		window        int
		wantCadence   int
		wantReleaseAt int
	}{
		{name: "minimum floor", window: 10_000, wantCadence: 1_500, wantReleaseAt: 9_000},
		{name: "adaptive ratio", window: 64_000, wantCadence: 1_600, wantReleaseAt: 49_600},
		{name: "adaptive cap", window: 200_000, wantCadence: 2_000, wantReleaseAt: 152_000},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{ModelContextLimit: tc.window, Nudge: nudge}
			cadence := NudgeGrowthFloor(cfg)
			if cadence != tc.wantCadence {
				t.Fatalf("suppression cadence = %d, want %d", cadence, tc.wantCadence)
			}

			pressureStart := int(float64(tc.window) * nudge.MaxContextLimitPct)
			releaseAt := pressureStart + cadence
			if releaseAt != tc.wantReleaseAt {
				t.Fatalf("suppression release point = %d, want %d", releaseAt, tc.wantReleaseAt)
			}
			if releaseAt > tc.window {
				t.Fatalf("suppression release at %d exceeds the configured %d-token window", releaseAt, tc.window)
			}
		})
	}
}

// The emergency band exists because at that pressure the next request may not
// fit at all. One nudge costs a couple of thousand tokens; an overflow costs the
// turn. So the failure ladder must not be able to silence the emergency voice.
func TestEmergencyPressureIsNudgedEvenWhenSuppressed(t *testing.T) {
	cfg := DefaultConfig(40_000)
	// 96% of the window: past EmergencyThresholdPct.
	tokens := int(float64(cfg.ModelContextLimit) * 0.96)

	d := DecideNudge(DecideNudgeInput{
		State:      CreateInitialState("s", ""),
		Config:     cfg,
		TokenCount: tokens,
		Recommendation: Recommendation{CompressibleRanges: []RangeInfo{{
			StartRef: "m00002", EndRef: "m00050", Count: 20,
			Tokens: 20_000, Chars: 80_000, ToolPct: 90, TextPct: 10,
		}}},
	})
	if !d.ShouldInject {
		t.Fatalf("no nudge at 96%% of the window even before any suppression; "+
			"the emergency band is not firing (reason: %s)", d.Reason)
	}
	if !d.Breakdown.Emergency {
		t.Fatalf("the decision at 96%% pressure is not flagged emergency: %+v", d.Breakdown)
	}

	// The host-side gate is what suppression acts on. It is a separate decision
	// from DecideNudge, and it currently has no knowledge of pressure at all —
	// see Session.nudgeAllowed, which branches only on attempts and growth.
	// This test documents the seam; the assertion belongs with the host, in
	// noaadapter.TestSuppressionSurvivesIntoTheEmergencyBand.
}
