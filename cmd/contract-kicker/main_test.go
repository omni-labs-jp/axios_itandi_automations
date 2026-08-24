package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/omni-labs-jp/axios_itandi_automations/internal/contractkicker"
	"github.com/omni-labs-jp/axios_itandi_automations/pkg/env"
	"github.com/omni-labs-jp/axios_itandi_automations/pkg/logging"
	"github.com/omni-labs-jp/axios_itandi_automations/pkg/webhook"
)

func loadConfig(t *testing.T, overrides map[string]string) contractkicker.Config {
	t.Helper()
	m := map[string]string{
		contractkicker.EnvITANDIToken:      "tok",
		contractkicker.EnvHerokuWebhookURL: "https://app.herokuapp.com/webhook/itandi/contract",
	}
	for k, v := range overrides {
		m[k] = v
	}
	cfg, err := contractkicker.LoadConfig(env.NewFromMap(m))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

func TestNewSenderRealClient(t *testing.T) {
	var buf bytes.Buffer
	log := logging.New(&buf, logging.Options{})

	s, err := newSender(loadConfig(t, nil), log)
	if err != nil {
		t.Fatalf("newSender: %v", err)
	}
	c, ok := s.(*webhook.Client)
	if !ok {
		t.Fatalf("sender = %T, want *webhook.Client", s)
	}
	if c.URL() != "https://app.herokuapp.com/webhook/itandi/contract" {
		t.Errorf("URL() = %q", c.URL())
	}
}

func TestNewSenderDryRun(t *testing.T) {
	var buf bytes.Buffer
	log := logging.New(&buf, logging.Options{})

	s, err := newSender(loadConfig(t, map[string]string{contractkicker.EnvDryRun: "true"}), log)
	if err != nil {
		t.Fatalf("newSender: %v", err)
	}
	if _, ok := s.(webhook.DryRunSender); !ok {
		t.Fatalf("sender = %T, want webhook.DryRunSender", s)
	}
	if !strings.Contains(buf.String(), "DRY_RUN is enabled") {
		t.Errorf("dry run should be announced loudly: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"severity":"WARNING"`) {
		t.Errorf("dry run notice should be a WARNING: %s", buf.String())
	}
}

func TestNewSenderRejectsBadURL(t *testing.T) {
	cfg := loadConfig(t, nil)
	cfg.HerokuWebhookURL = "not-a-url"

	if _, err := newSender(cfg, slog.Default()); err == nil {
		t.Fatal("expected an error for an invalid webhook URL")
	}
}

func TestNewSenderAddsAuthHeader(t *testing.T) {
	cfg := loadConfig(t, map[string]string{
		contractkicker.EnvHerokuAuthHeader: "X-Kicker-Token",
		contractkicker.EnvHerokuAuthToken:  "s3cret",
	})
	s, err := newSender(cfg, logging.New(&bytes.Buffer{}, logging.Options{}))
	if err != nil {
		t.Fatalf("newSender: %v", err)
	}
	if _, ok := s.(*webhook.Client); !ok {
		t.Fatalf("sender = %T", s)
	}
}
