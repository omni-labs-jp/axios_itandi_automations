package contractkicker

import (
	"strings"
	"testing"
	"time"

	"github.com/omni-labs-jp/axios_itandi_automations/pkg/env"
	"github.com/omni-labs-jp/axios_itandi_automations/pkg/itandi"
	"github.com/omni-labs-jp/axios_itandi_automations/pkg/ledger"
)

func minimalEnv() map[string]string {
	return map[string]string{
		EnvITANDIToken:      "tok",
		EnvHerokuWebhookURL: "https://app.herokuapp.com/webhook/itandi/contract",
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := LoadConfig(env.NewFromMap(minimalEnv()))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.ITANDIBaseURL != itandi.ProductionBaseURL {
		t.Errorf("ITANDIBaseURL = %q", cfg.ITANDIBaseURL)
	}
	if cfg.TargetStatusID != itandi.StatusContract {
		t.Errorf("TargetStatusID = %d, want %d", cfg.TargetStatusID, itandi.StatusContract)
	}
	if cfg.Lookback != 24*time.Hour {
		t.Errorf("Lookback = %s", cfg.Lookback)
	}
	if cfg.MaxAttempts != 3 {
		t.Errorf("MaxAttempts = %d", cfg.MaxAttempts)
	}
	if cfg.SendLimitPerRun != 1 {
		t.Errorf("SendLimitPerRun = %d", cfg.SendLimitPerRun)
	}
	if cfg.RequestTimeout != 30*time.Second {
		t.Errorf("RequestTimeout = %s", cfg.RequestTimeout)
	}
	if cfg.DryRun {
		t.Error("DryRun should default to false")
	}
	if cfg.FirestoreNamespace != "contract-kicker" {
		t.Errorf("FirestoreNamespace = %q", cfg.FirestoreNamespace)
	}
	if cfg.FirestoreKind != ledger.DefaultKind {
		t.Errorf("FirestoreKind = %q", cfg.FirestoreKind)
	}
	if got := cfg.Backoff.Delay(1); got != 15*time.Minute {
		t.Errorf("backoff Delay(1) = %s, want 15m", got)
	}
	if got := cfg.Backoff.Delay(3); got != 4*time.Hour {
		t.Errorf("backoff Delay(3) = %s, want 4h", got)
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	m := minimalEnv()
	m[EnvITANDIBaseURL] = itandi.StagingBaseURL
	m[EnvTargetStatusID] = "105"
	m[EnvLookbackHours] = "48"
	m[EnvMaxAttempts] = "5"
	m[EnvSendLimitPerRun] = "3"
	m[EnvRequestTimeoutMS] = "5000"
	m[EnvDryRun] = "true"
	m[EnvRetrySchedule] = "1m,2m"
	m[EnvFirestoreNamespace] = "ns"
	m[EnvFirestoreKind] = "kind"
	m[EnvFirestoreDatabaseID] = "db"
	m[EnvGCPProject] = "proj"
	m[EnvLogLevel] = "debug"
	m[EnvHerokuAuthHeader] = "X-Kicker-Token"
	m[EnvHerokuAuthToken] = "s3cret"

	cfg, err := LoadConfig(env.NewFromMap(m))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.ITANDIBaseURL != itandi.StagingBaseURL || cfg.TargetStatusID != 105 ||
		cfg.Lookback != 48*time.Hour || cfg.MaxAttempts != 5 || cfg.SendLimitPerRun != 3 ||
		cfg.RequestTimeout != 5*time.Second || !cfg.DryRun ||
		cfg.FirestoreNamespace != "ns" || cfg.FirestoreKind != "kind" ||
		cfg.FirestoreDatabaseID != "db" || cfg.GCPProjectID != "proj" || cfg.LogLevel != "debug" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if got := cfg.Backoff.Delay(2); got != 2*time.Minute {
		t.Errorf("backoff Delay(2) = %s, want 2m", got)
	}
}

func TestLoadConfigReportsAllMissingValuesAtOnce(t *testing.T) {
	_, err := LoadConfig(env.NewFromMap(map[string]string{}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, key := range []string{EnvITANDIToken, EnvHerokuWebhookURL} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error should mention %s: %v", key, err)
		}
	}
}

func TestLoadConfigRejectsInvalidValues(t *testing.T) {
	cases := map[string]map[string]string{
		"zero lookback":      {EnvLookbackHours: "0"},
		"negative attempts":  {EnvMaxAttempts: "-1"},
		"zero send limit":    {EnvSendLimitPerRun: "0"},
		"bad timeout":        {EnvRequestTimeoutMS: "abc"},
		"bad dry run":        {EnvDryRun: "maybe"},
		"bad retry schedule": {EnvRetrySchedule: "15m,nope"},
		"auth header only":   {EnvHerokuAuthHeader: "X-Kicker-Token"},
		"auth token only":    {EnvHerokuAuthToken: "s3cret"},
	}
	for name, overrides := range cases {
		t.Run(name, func(t *testing.T) {
			m := minimalEnv()
			for k, v := range overrides {
				m[k] = v
			}
			if _, err := LoadConfig(env.NewFromMap(m)); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

func TestValidateCatchesHandBuiltConfig(t *testing.T) {
	base, err := LoadConfig(env.NewFromMap(minimalEnv()))
	if err != nil {
		t.Fatal(err)
	}

	mutate := map[string]func(*Config){
		"no base url": func(c *Config) { c.ITANDIBaseURL = "" },
		"no token":    func(c *Config) { c.ITANDIToken = "" },
		"no webhook":  func(c *Config) { c.HerokuWebhookURL = "" },
		"no lookback": func(c *Config) { c.Lookback = 0 },
		"no attempts": func(c *Config) { c.MaxAttempts = 0 },
		"no limit":    func(c *Config) { c.SendLimitPerRun = 0 },
		"no timeout":  func(c *Config) { c.RequestTimeout = 0 },
		"no backoff":  func(c *Config) { c.Backoff.Schedule = nil },
	}
	for name, f := range mutate {
		cfg := base
		f(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate should fail: %s", name)
		}
	}
	if err := base.Validate(); err != nil {
		t.Errorf("baseline config should be valid: %v", err)
	}
}

func TestLogAttrsHidesSecrets(t *testing.T) {
	m := minimalEnv()
	m[EnvITANDIToken] = "super-secret-token"
	m[EnvHerokuAuthToken] = "s3cret"
	m[EnvHerokuAuthHeader] = "X-Kicker-Token"
	cfg, err := LoadConfig(env.NewFromMap(m))
	if err != nil {
		t.Fatal(err)
	}

	for _, v := range cfg.LogAttrs() {
		if s, ok := v.(string); ok {
			if strings.Contains(s, "super-secret-token") || strings.Contains(s, "s3cret") {
				t.Fatalf("LogAttrs leaked a secret: %v", cfg.LogAttrs())
			}
		}
	}
}
