package integration

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
)

// TestECSSpendCapConfigured verifies the #36 spend-cap + event-bus wiring is
// present on the deployed stack: the SSM config JSON carries
// max_spend_per_window / spend_window / event_bus_name, and the queue-drain
// Lambda's environment carries MAX_SPEND_PER_WINDOW / SPEND_WINDOW_SECONDS /
// MAX_CONCURRENT. This is a config-shape assertion — no run is launched, so it
// is cheap and fast.
//
// It does NOT force a realized-spend exceed: the e2e script workflows cost ~$0,
// so realized spend never crosses the (generous) cap. The gating logic itself
// is unit-covered (realizedSpendGate); this proves the deployment plumbing.
//
// The e2e CDK app (cdk/e2e/app.ts) sets maxSpendPerWindow:1000 +
// spendWindow:24h, so these exact values are asserted. If you change the app,
// update the expectations here.
func TestECSSpendCapConfigured(t *testing.T) {
	t.Parallel()
	h := newECSHarness(t)
	d := ecsd(t, h)
	cfg := d.cfg

	// --- SSM config carries the spend cap + bus name ---
	if cfg.MaxSpendPerWindow != 1000 {
		t.Errorf("SSM max_spend_per_window = %v, want 1000", cfg.MaxSpendPerWindow)
	}
	if cfg.SpendWindow != "86400s" {
		t.Errorf("SSM spend_window = %q, want %q (24h in seconds)", cfg.SpendWindow, "86400s")
	}
	if cfg.EventBusName == "" {
		t.Error("SSM event_bus_name is empty; expected the custom horde bus name")
	}
	if !strings.HasPrefix(cfg.EventBusName, "horde-") {
		t.Errorf("SSM event_bus_name = %q, want a horde-<slug> bus", cfg.EventBusName)
	}

	// --- Drain Lambda env carries the spend cap + concurrency ---
	// The drain function is named `<event-bus-name>-queue-drain` because both
	// the bus and the function are slug-namespaced as `horde-<slug>`.
	fnName := cfg.EventBusName + "-queue-drain"
	lc := lambda.NewFromConfig(d.awsCfg)
	out, err := lc.GetFunctionConfiguration(d.ctx, &lambda.GetFunctionConfigurationInput{
		FunctionName: aws.String(fnName),
	})
	if err != nil {
		t.Fatalf("GetFunctionConfiguration(%s): %v", fnName, err)
	}
	if out.Environment == nil || out.Environment.Variables == nil {
		t.Fatalf("drain Lambda %s has no environment variables", fnName)
	}
	env := out.Environment.Variables
	checkEnv := func(key, want string) {
		if got := env[key]; got != want {
			t.Errorf("drain Lambda env[%s] = %q, want %q", key, got, want)
		}
	}
	checkEnv("MAX_SPEND_PER_WINDOW", "1000")
	checkEnv("SPEND_WINDOW_SECONDS", "86400")
	checkEnv("MAX_CONCURRENT", "20")
	if env["EVENT_BUS_NAME"] == "" {
		t.Error("drain Lambda env EVENT_BUS_NAME is empty")
	}
	if env["RUNS_TABLE"] == "" {
		t.Error("drain Lambda env RUNS_TABLE is empty")
	}
}
